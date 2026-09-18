package core

import (
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"runtime"
	"slices"

	"github.com/encodeous/nylon/polyamide/tun"
)

// SysApplyOps is the set of OS-level route/address operations the applier runs.
// Tests may substitute a slow implementation through AuxConfig["sys_apply_ops"].
type SysApplyOps struct {
	ConfigureAlias func(logger *slog.Logger, ifName string, addr netip.Addr) error
	RemoveAlias    func(logger *slog.Logger, ifName string, addr netip.Addr) error
	ConfigureRoute func(logger *slog.Logger, dev tun.Device, ifName string, route netip.Prefix) error
	RemoveRoute    func(logger *slog.Logger, dev tun.Device, ifName string, route netip.Prefix) error
}

type sysApplyRequest struct {
	desiredAliases []netip.Addr
	desiredRoutes  []netip.Prefix
	appliedAliases []netip.Addr
	appliedRoutes  []netip.Prefix
}

type sysApplyOutcome struct {
	appliedAliases []netip.Addr
	appliedRoutes  []netip.Prefix
	err            error
}

// requestSystemApply hands the desired OS-level state to the applier goroutine.
// It must only be called from the dispatch loop: sysApply.inFlight and
// sysApply.pending are owned by it. The request channel holds exactly one
// request, so a reconciliation that is already in flight only marks the
// incoming one as pending instead of blocking the caller.
func (n *Nylon) requestSystemApply() {
	if n.NoNetConfigure || n.NoTun || n.Device == nil {
		return
	}
	if n.sysApply.inFlight {
		n.sysApply.pending = true
		return
	}
	n.sysApply.inFlight = true
	n.sysApply.requests <- sysApplyRequest{
		desiredAliases: slices.Clone(n.GetRouter(n.LocalCfg.Id).Addresses),
		desiredRoutes:  n.ComputeSysRouteTable(),
		appliedAliases: slices.Clone(n.AppliedSystem.Aliases),
		appliedRoutes:  slices.Clone(n.AppliedSystem.Routes),
	}
}

// RoutineSystemApply performs the OS-level reconciliation on its own goroutine
// so that programming routes and aliases never stalls the dispatch loop.
func (n *Nylon) RoutineSystemApply() {
	defer n.sysApply.stopped.Done()
	for {
		select {
		case <-n.sysApply.stop:
			return
		case req := <-n.sysApply.requests:
			out := applySystemRequest(n, req)
			select {
			case n.sysApply.outcomes <- out:
			case <-n.sysApply.stop:
				return
			}
		}
	}
}

// applySystemRequest performs the OS reconciliation for req and returns the new
// applied state. It never reads or writes Nylon state.
func applySystemRequest(n *Nylon, req sysApplyRequest) sysApplyOutcome {
	ops := n.sysApply.ops
	appliedAliases := slices.Clone(req.appliedAliases)
	appliedRoutes := slices.Clone(req.appliedRoutes)
	var syncErr error
	// we must first add the new alias before removing the old ones, else the system might flush our routes
	for _, newEntry := range req.desiredAliases {
		if !slices.Contains(appliedAliases, newEntry) {
			n.Log.Debug("installing alias", "addr", newEntry.String())
			err := ops.ConfigureAlias(n.Log, n.Interface, newEntry)
			if err != nil {
				n.Log.Error("failed to configure alias", "err", err)
				syncErr = errors.Join(syncErr, fmt.Errorf("install alias %s: %w", newEntry, err))
				continue
			}
			appliedAliases = append(appliedAliases, newEntry)
		}
	}
	hadAliases := len(appliedAliases) != 0
	for _, oldEntry := range slices.Clone(appliedAliases) {
		if !slices.Contains(req.desiredAliases, oldEntry) {
			n.Log.Debug("removing old alias", "addr", oldEntry.String())
			err := ops.RemoveAlias(n.Log, n.Interface, oldEntry)
			if err != nil {
				n.Log.Error("failed to remove alias", "err", err)
				syncErr = errors.Join(syncErr, fmt.Errorf("remove alias %s: %w", oldEntry, err))
				continue
			}
			appliedAliases = slices.DeleteFunc(appliedAliases, func(addr netip.Addr) bool {
				return addr == oldEntry
			})
		}
	}
	// special case for linux: if all aliases are removed, the kernel will also flush the routes
	if hadAliases && len(appliedAliases) == 0 && runtime.GOOS == "linux" {
		appliedRoutes = nil
	}
	// Install new routes before removing old ones so a partial reconciliation
	// preserves as much connectivity as possible.
	for _, newEntry := range req.desiredRoutes {
		if !slices.Contains(appliedRoutes, newEntry) {
			// install route
			n.Log.Debug("installing new route", "prefix", newEntry.String())
			err := ops.ConfigureRoute(n.Log, n.Tun, n.Interface, newEntry)
			if err != nil {
				n.Log.Error("failed to configure route", "err", err)
				syncErr = errors.Join(syncErr, fmt.Errorf("install route %s: %w", newEntry, err))
				continue
			}
			appliedRoutes = append(appliedRoutes, newEntry)
		}
	}
	for _, oldEntry := range slices.Clone(appliedRoutes) {
		if !slices.Contains(req.desiredRoutes, oldEntry) {
			// uninstall route
			n.Log.Debug("removing old route", "prefix", oldEntry.String())
			err := ops.RemoveRoute(n.Log, n.Tun, n.Interface, oldEntry)
			if err != nil {
				n.Log.Error("failed to remove route", "err", err)
				syncErr = errors.Join(syncErr, fmt.Errorf("remove route %s: %w", oldEntry, err))
				continue
			}
			appliedRoutes = slices.DeleteFunc(appliedRoutes, func(prefix netip.Prefix) bool {
				return prefix == oldEntry
			})
		}
	}
	return sysApplyOutcome{
		appliedAliases: appliedAliases,
		appliedRoutes:  appliedRoutes,
		err:            syncErr,
	}
}
