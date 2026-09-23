package core

import (
	"errors"
	"log/slog"
	"net/netip"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/encodeous/nylon/polyamide/device"
	"github.com/encodeous/nylon/polyamide/tun"
)

// sysApplyRecorder returns SysApplyOps that append every invocation to calls.
// The zero values of failAlias/failRoute never match, i.e. "fail nothing".
func sysApplyRecorder(calls *[]string, failAlias netip.Addr, failRoute netip.Prefix) SysApplyOps {
	return SysApplyOps{
		ConfigureAlias: func(_ *slog.Logger, _ string, addr netip.Addr) error {
			*calls = append(*calls, "add-alias "+addr.String())
			if addr == failAlias {
				return errors.New("alias add failed")
			}
			return nil
		},
		RemoveAlias: func(_ *slog.Logger, _ string, addr netip.Addr) error {
			*calls = append(*calls, "del-alias "+addr.String())
			return nil
		},
		ConfigureRoute: func(_ *slog.Logger, _ tun.Device, _ string, route netip.Prefix) error {
			*calls = append(*calls, "add-route "+route.String())
			if route == failRoute {
				return errors.New("route add failed")
			}
			return nil
		},
		RemoveRoute: func(_ *slog.Logger, _ tun.Device, _ string, route netip.Prefix) error {
			*calls = append(*calls, "del-route "+route.String())
			return nil
		},
	}
}

func TestApplySystemRequestOrderAndPartialFailure(t *testing.T) {
	t.Run("adds run before removes and applied state tracks successes", func(t *testing.T) {
		keptAlias := netip.MustParseAddr("10.0.0.2")
		staleAlias := netip.MustParseAddr("10.0.0.3")
		newAlias := netip.MustParseAddr("10.0.0.4")
		keptRoute := netip.MustParsePrefix("10.2.0.0/24")
		staleRoute := netip.MustParsePrefix("10.1.0.0/24")
		newRoute := netip.MustParsePrefix("10.3.0.0/24")

		var calls []string
		n := testNylonWithPrefixes()
		n.Interface = "nylontest0"
		// the new alias and the new route both fail: everything else must still
		// converge and only the successful operations may enter the applied state
		n.sysApply.ops = sysApplyRecorder(&calls, newAlias, newRoute)

		out := applySystemRequest(n, sysApplyRequest{
			desiredAliases: []netip.Addr{keptAlias, newAlias},
			desiredRoutes:  []netip.Prefix{keptRoute, newRoute},
			appliedAliases: []netip.Addr{keptAlias, staleAlias},
			appliedRoutes:  []netip.Prefix{keptRoute, staleRoute},
		})

		wantCalls := []string{
			"add-alias 10.0.0.4",
			"del-alias 10.0.0.3",
			"add-route 10.3.0.0/24",
			"del-route 10.1.0.0/24",
		}
		if !slices.Equal(calls, wantCalls) {
			t.Fatalf("call order = %v, want %v", calls, wantCalls)
		}
		aliasAdd, aliasDel := slices.Index(calls, wantCalls[0]), slices.Index(calls, wantCalls[1])
		if aliasAdd == -1 || aliasDel == -1 || aliasAdd > aliasDel {
			t.Errorf("new aliases must be installed before stale ones are removed, got %v", calls)
		}
		routeAdd, routeDel := slices.Index(calls, wantCalls[2]), slices.Index(calls, wantCalls[3])
		if routeAdd == -1 || routeDel == -1 || routeAdd > routeDel {
			t.Errorf("new routes must be installed before stale ones are removed, got %v", calls)
		}

		if want := []netip.Addr{keptAlias}; !slices.Equal(out.appliedAliases, want) {
			t.Errorf("applied aliases = %v, want %v", out.appliedAliases, want)
		}
		if want := []netip.Prefix{keptRoute}; !slices.Equal(out.appliedRoutes, want) {
			t.Errorf("applied routes = %v, want %v", out.appliedRoutes, want)
		}
		if out.err == nil {
			t.Fatal("expected the failed operations to be reported")
		}
		for _, wantMsg := range []string{"install alias 10.0.0.4", "install route 10.3.0.0/24"} {
			if !strings.Contains(out.err.Error(), wantMsg) {
				t.Errorf("error %q does not mention %q", out.err, wantMsg)
			}
		}
	})

	t.Run("removing the last alias drops the applied route state on linux", func(t *testing.T) {
		staleAlias := netip.MustParseAddr("10.0.0.5")
		staleRoute := netip.MustParsePrefix("10.1.0.0/24")

		var calls []string
		n := testNylonWithPrefixes()
		n.Interface = "nylontest0"
		n.sysApply.ops = sysApplyRecorder(&calls, netip.Addr{}, netip.Prefix{})

		out := applySystemRequest(n, sysApplyRequest{
			// no desired aliases: the only one is stale and gets removed
			appliedAliases: []netip.Addr{staleAlias},
			appliedRoutes:  []netip.Prefix{staleRoute},
		})

		if len(out.appliedAliases) != 0 {
			t.Errorf("applied aliases = %v, want none", out.appliedAliases)
		}
		// the linux kernel flushes the interface routes when its last address
		// disappears, so the applied route state must be dropped without
		// issuing per-route deletions. Other platforms keep the routes.
		if runtime.GOOS == "linux" {
			if !slices.Equal(calls, []string{"del-alias 10.0.0.5"}) {
				t.Fatalf("calls = %v, want only the alias removal after the kernel route flush", calls)
			}
			if out.appliedRoutes != nil {
				t.Errorf("applied routes = %v, want nil after the kernel route flush", out.appliedRoutes)
			}
		} else {
			if !slices.Equal(calls, []string{"del-alias 10.0.0.5", "del-route 10.1.0.0/24"}) {
				t.Fatalf("calls = %v, want the alias and the route removal", calls)
			}
			if len(out.appliedRoutes) != 0 {
				t.Errorf("applied routes = %v, want the stale route removed", out.appliedRoutes)
			}
		}
	})
}

func TestSystemApplyDoesNotBlockDispatch(t *testing.T) {
	const opDelay = 200 * time.Millisecond

	n := testNylonWithPrefixes()
	n.Interface = "nylontest0"
	n.Device = &device.Device{}
	n.router.log = n.Log
	// a stale alias guarantees the applier performs real work, so the in-flight
	// window lasts the full fake operation delay
	n.AppliedSystem.Aliases = []netip.Addr{netip.MustParseAddr("10.0.0.5")}

	var applied atomic.Bool
	slowAlias := func(_ *slog.Logger, _ string, _ netip.Addr) error {
		time.Sleep(opDelay)
		applied.Store(true)
		return nil
	}
	slowRoute := func(_ *slog.Logger, _ tun.Device, _ string, _ netip.Prefix) error {
		time.Sleep(opDelay)
		applied.Store(true)
		return nil
	}
	n.sysApply.ops = SysApplyOps{
		ConfigureAlias: slowAlias,
		RemoveAlias:    slowAlias,
		ConfigureRoute: slowRoute,
		RemoveRoute:    slowRoute,
	}
	n.sysApply.requests = make(chan sysApplyRequest, 1)
	n.sysApply.outcomes = make(chan sysApplyOutcome, 1)
	n.sysApply.stop = make(chan struct{})
	n.sysApply.stopped.Add(1)
	go n.RoutineSystemApply()
	defer func() {
		close(n.sysApply.stop)
		n.sysApply.stopped.Wait()
	}()

	start := time.Now()
	n.requestSystemApply()
	if elapsed := time.Since(start); elapsed >= 50*time.Millisecond {
		t.Fatalf("requestSystemApply took %v, want it to hand the work off without waiting", elapsed)
	}
	if !n.sysApply.inFlight {
		t.Fatal("expected the first request to be in flight")
	}

	start = time.Now()
	n.requestSystemApply()
	if elapsed := time.Since(start); elapsed >= 50*time.Millisecond {
		t.Fatalf("second requestSystemApply took %v, want it to return immediately", elapsed)
	}
	if !n.sysApply.inFlight || !n.sysApply.pending {
		t.Fatalf("inFlight = %v, pending = %v, want true/true", n.sysApply.inFlight, n.sysApply.pending)
	}

	select {
	case out := <-n.sysApply.outcomes:
		if out.err != nil {
			t.Fatalf("unexpected apply error: %v", out.err)
		}
		if len(out.appliedAliases) != 0 {
			t.Errorf("applied aliases = %v, want the stale alias removed", out.appliedAliases)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the apply outcome")
	}
	if !applied.Load() {
		t.Fatal("expected the slow fake operations to have run")
	}
}
