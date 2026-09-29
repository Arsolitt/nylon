package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
)

// spkBinder is an in-memory AddrBinder standing in for the netlink
// implementation (which needs root): it records every Ensure/Remove call and
// serves ListInPool from a live set.
type spkBinder struct {
	mu      sync.Mutex
	live    map[netip.Addr]struct{}
	ensured []netip.Addr
	removed []netip.Addr
}

func newSpkBinder() *spkBinder {
	return &spkBinder{live: make(map[netip.Addr]struct{})}
}

func (b *spkBinder) Ensure(ip netip.Addr) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ensured = append(b.ensured, ip)
	b.live[ip] = struct{}{}
	return nil
}

func (b *spkBinder) Remove(ip netip.Addr) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.removed = append(b.removed, ip)
	delete(b.live, ip)
	return nil
}

func (b *spkBinder) ListInPool() ([]netip.Addr, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	ips := make([]netip.Addr, 0, len(b.live))
	for ip := range b.live {
		ips = append(ips, ip)
	}
	slices.SortFunc(ips, netip.Addr.Compare)
	return ips, nil
}

func (b *spkBinder) ensuredList() []netip.Addr {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.ensured)
}

func (b *spkBinder) removedList() []netip.Addr {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.removed)
}

func (b *spkBinder) contains(ip netip.Addr) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.live[ip]
	return ok
}

// spkHarness wires a fake clientset, an informer factory (100 ms resync), a
// fake binder and a Speaker over a temp prefixes dir, all running, with
// teardown handled by t.Cleanup.
type spkHarness struct {
	t       *testing.T
	client  *fake.Clientset
	factory informers.SharedInformerFactory
	binder  *spkBinder
	pools   *PoolSet
	dir     string
	speaker *Speaker
	cancel  context.CancelFunc
	done    chan struct{}
}

func newSpkHarness(t *testing.T, nodeName string) *spkHarness {
	t.Helper()
	return newSpkHarnessWithClass(t, nodeName, "")
}

// newSpkHarnessWithClass is newSpkHarness with --lb-class set to lbClass,
// scoping the speaker to Services carrying that spec.loadBalancerClass.
func newSpkHarnessWithClass(t *testing.T, nodeName, lbClass string) *spkHarness {
	t.Helper()
	return newSpkHarnessWithClient(t, fake.NewSimpleClientset(), nodeName, lbClass, 0)
}

// newSpkHarnessWithClient wires a Speaker over an existing clientset, so
// several harnesses can simulate several nodes observing one cluster state.
// drainDelay is applied before the speaker starts — writing it later would
// race with the reconcile loop. Zero keeps the immediate-unbind behaviour the
// withdrawal tests assert; the drain tests pass a non-zero window.
func newSpkHarnessWithClient(t *testing.T, client *fake.Clientset, nodeName, lbClass string, drainDelay time.Duration) *spkHarness {
	t.Helper()
	h := &spkHarness{
		t:      t,
		client: client,
		binder: newSpkBinder(),
		dir:    t.TempDir(),
		done:   make(chan struct{}),
	}
	pools, err := ParsePoolSet([]string{"shared=10.110.0.0/24", "private=198.51.100.0/24"}, []string{"10.110.0.1"})
	assert.NoError(t, err)
	h.pools = pools
	h.factory = informers.NewSharedInformerFactory(h.client, 100*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	speaker, err := NewSpeaker(SpeakerOptions{
		Client:      h.client,
		Factory:     h.factory,
		Pools:       h.pools,
		LBClass:     lbClass,
		PrefixesDir: h.dir,
		NodeName:    nodeName,
		Binder:      h.binder,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Resync:      100 * time.Millisecond,
	})
	assert.NoError(t, err)
	speaker.drainDelay = drainDelay
	h.speaker = speaker

	h.factory.Start(ctx.Done())
	go func() {
		defer close(h.done)
		_ = speaker.Run(ctx)
	}()
	t.Cleanup(func() {
		h.cancel()
		select {
		case <-h.done:
		case <-time.After(5 * time.Second):
			t.Error("speaker did not stop within 5s of context cancellation")
		}
	})
	return h
}

func (h *spkHarness) createService(svc *corev1.Service) {
	h.t.Helper()
	_, err := h.client.CoreV1().Services(svc.Namespace).Create(context.Background(), svc, metav1.CreateOptions{})
	assert.NoError(h.t, err)
}

func (h *spkHarness) createSlice(slice *discoveryv1.EndpointSlice) {
	h.t.Helper()
	_, err := h.client.DiscoveryV1().EndpointSlices(slice.Namespace).Create(context.Background(), slice, metav1.CreateOptions{})
	assert.NoError(h.t, err)
}

func (h *spkHarness) updateSlice(slice *discoveryv1.EndpointSlice) {
	h.t.Helper()
	_, err := h.client.DiscoveryV1().EndpointSlices(slice.Namespace).Update(context.Background(), slice, metav1.UpdateOptions{})
	assert.NoError(h.t, err)
}

func (h *spkHarness) deleteSlice(ns, name string) {
	h.t.Helper()
	err := h.client.DiscoveryV1().EndpointSlices(ns).Delete(context.Background(), name, metav1.DeleteOptions{})
	assert.NoError(h.t, err)
}

// spkService builds a type=LoadBalancer Service whose status already carries
// ingressIP — the allocator is a different component; the speaker only
// consumes its output. An empty policy string is the k8s default (Cluster).
func spkService(ns, name, ingressIP string, policy corev1.ServiceExternalTrafficPolicyType) *corev1.Service {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: corev1.ServiceSpec{
			Type:                  corev1.ServiceTypeLoadBalancer,
			ExternalTrafficPolicy: policy,
		},
	}
	if ingressIP != "" {
		svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: ingressIP}}
	}
	return svc
}

// spkSlice builds an EndpointSlice labelled for svcName with a single
// endpoint on nodeName; ready is Conditions.Ready verbatim (nil means
// ready per k8s semantics).
func spkSlice(ns, name, svcName, nodeName string, ready *bool) *discoveryv1.EndpointSlice {
	return &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      name,
			Labels:    map[string]string{discoveryv1.LabelServiceName: svcName},
		},
		Endpoints: []discoveryv1.Endpoint{{
			NodeName:   &nodeName,
			Conditions: discoveryv1.EndpointConditions{Ready: ready},
		}},
	}
}

// spkWriteAnnounceFile writes a valid announce file directly into dir,
// standing in for state left behind by a previous speaker run.
func spkWriteAnnounceFile(t *testing.T, dir, file string, ip netip.Addr) {
	t.Helper()
	metric := uint32(0)
	content, err := json.Marshal(lbPrefixFile{
		Version: 1,
		Prefixes: []lbPrefixEntry{{
			Type:   "static",
			Prefix: netip.PrefixFrom(ip, 32).Masked().String(),
			Metric: &metric,
		}},
	})
	assert.NoError(t, err)
	assert.NoError(t, os.WriteFile(filepath.Join(dir, file), append(content, '\n'), 0o644))
}

// spkGolden returns the exact bytes writePrefixFile produces for ip:
// compact JSON plus a trailing newline.
func spkGolden(ip netip.Addr) string {
	return `{"version":1,"prefixes":[{"type":"static","prefix":"` + ip.String() + `/32","metric":0}]}` + "\n"
}

func spkExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// TestSpeakerClusterPolicyAnnouncesAndBinds: with the default (Cluster)
// externalTrafficPolicy every node announces the ingress /32 — the announce
// file appears with exactly the golden bytes and the binder recorded the
// address.
func TestSpeakerClusterPolicyAnnouncesAndBinds(t *testing.T) {
	h := newSpkHarness(t, "uk-node-1")
	ip := mustAddr(t, "10.110.0.10")

	h.createService(spkService("default", "web", ip.String(), ""))

	path := filepath.Join(h.dir, fileName("default", "web"))
	assert.Eventually(t, func() bool {
		data, err := os.ReadFile(path)
		return err == nil && string(data) == spkGolden(ip)
	}, 2*time.Second, 10*time.Millisecond, "announce file should hold exactly the golden bytes")
	assert.Eventually(t, func() bool {
		return slices.Contains(h.binder.ensuredList(), ip)
	}, 2*time.Second, 10*time.Millisecond, "binder should have ensured the ingress IP")
	assert.True(t, h.binder.contains(ip), "address should be live after Ensure")
}

// TestSpeakerLocalPolicyUnreadyEndpointWithdraws: Local policy announces
// while a local endpoint is ready; marking it not-ready removes the announce
// file and unbinds the address.
func TestSpeakerLocalPolicyUnreadyEndpointWithdraws(t *testing.T) {
	h := newSpkHarness(t, "uk-node-1")
	ip := mustAddr(t, "10.110.0.11")

	h.createService(spkService("default", "api", ip.String(), corev1.ServiceExternalTrafficPolicyLocal))
	ready := true
	h.createSlice(spkSlice("default", "api-abc", "api", "uk-node-1", &ready))

	path := filepath.Join(h.dir, fileName("default", "api"))
	assert.Eventually(t, func() bool { return spkExists(path) },
		2*time.Second, 10*time.Millisecond, "ready local endpoint should announce")

	notReady := false
	h.updateSlice(spkSlice("default", "api-abc", "api", "uk-node-1", &notReady))

	assert.Eventually(t, func() bool { return !spkExists(path) },
		2*time.Second, 10*time.Millisecond, "not-ready endpoint should withdraw the announce")
	assert.Eventually(t, func() bool {
		return slices.Contains(h.binder.removedList(), ip)
	}, 2*time.Second, 10*time.Millisecond, "withdrawal should unbind the address")
	assert.False(t, h.binder.contains(ip), "address should be gone after withdrawal")
}

// TestSpeakerLocalPolicySliceDeletionWithdraws: deleting the EndpointSlice
// backing a Local service withdraws the announce.
func TestSpeakerLocalPolicySliceDeletionWithdraws(t *testing.T) {
	h := newSpkHarness(t, "uk-node-1")
	ip := mustAddr(t, "10.110.0.12")

	h.createService(spkService("default", "db", ip.String(), corev1.ServiceExternalTrafficPolicyLocal))
	ready := true
	h.createSlice(spkSlice("default", "db-xyz", "db", "uk-node-1", &ready))

	path := filepath.Join(h.dir, fileName("default", "db"))
	assert.Eventually(t, func() bool { return spkExists(path) },
		2*time.Second, 10*time.Millisecond, "ready local endpoint should announce")

	h.deleteSlice("default", "db-xyz")

	assert.Eventually(t, func() bool { return !spkExists(path) },
		2*time.Second, 10*time.Millisecond, "slice deletion should withdraw the announce")
}

// TestSpeakerGhostFileGarbageCollected: an lb-*.json orphaned by a Service
// that no longer exists is removed at the next reconcile pass and its
// address unbound.
func TestSpeakerGhostFileGarbageCollected(t *testing.T) {
	h := newSpkHarness(t, "uk-node-1")
	ghost := mustAddr(t, "10.110.0.99")
	spkWriteAnnounceFile(t, h.dir, "lb-default-ghost.json", ghost)

	path := filepath.Join(h.dir, "lb-default-ghost.json")
	assert.Eventually(t, func() bool { return !spkExists(path) },
		2*time.Second, 10*time.Millisecond, "ghost announce should be garbage-collected")
	assert.Eventually(t, func() bool {
		return slices.Contains(h.binder.removedList(), ghost)
	}, 2*time.Second, 10*time.Millisecond, "withdrawn ghost announce should unbind its address")
}

// TestSpeakerLocalPolicyForeignNodeNeverAnnounces: a ready endpoint on a
// different node must not announce here, checked over a window spanning
// several resync cycles.
func TestSpeakerLocalPolicyForeignNodeNeverAnnounces(t *testing.T) {
	h := newSpkHarness(t, "uk-node-1")
	ip := mustAddr(t, "10.110.0.13")

	h.createService(spkService("default", "cache", ip.String(), corev1.ServiceExternalTrafficPolicyLocal))
	ready := true
	h.createSlice(spkSlice("default", "cache-1", "cache", "nl-node-2", &ready))

	path := filepath.Join(h.dir, fileName("default", "cache"))
	assert.Never(t, func() bool { return spkExists(path) }, time.Second, 10*time.Millisecond,
		"a foreign ready endpoint must never announce on this node")
	assert.Never(t, func() bool { return len(h.binder.ensuredList()) > 0 }, time.Second, 10*time.Millisecond,
		"the binder must never be called for a foreign endpoint")
}

// TestSpeakerLocalPolicyNilReadyMeansReady: an endpoint whose
// Conditions.Ready is nil is treated as ready (k8s semantics).
func TestSpeakerLocalPolicyNilReadyMeansReady(t *testing.T) {
	h := newSpkHarness(t, "uk-node-1")
	ip := mustAddr(t, "10.110.0.14")

	h.createService(spkService("default", "queue", ip.String(), corev1.ServiceExternalTrafficPolicyLocal))
	h.createSlice(spkSlice("default", "queue-1", "queue", "uk-node-1", nil))

	path := filepath.Join(h.dir, fileName("default", "queue"))
	assert.Eventually(t, func() bool { return spkExists(path) },
		2*time.Second, 10*time.Millisecond, "a nil Conditions.Ready counts as ready")
	assert.Eventually(t, func() bool {
		return slices.Contains(h.binder.ensuredList(), ip)
	}, 2*time.Second, 10*time.Millisecond, "binder should have ensured the ingress IP")
}

// A class-scoped speaker announces and binds its own class's Service like
// the claim-all speaker does.
func TestSpeakerAnnouncesMatchingClassService(t *testing.T) {
	h := newSpkHarnessWithClass(t, "uk-node-1", "nylon")
	ip := mustAddr(t, "10.110.0.16")

	svc := spkService("default", "web", ip.String(), "")
	c := "nylon"
	svc.Spec.LoadBalancerClass = &c
	h.createService(svc)

	path := filepath.Join(h.dir, fileName("default", "web"))
	assert.Eventually(t, func() bool {
		data, err := os.ReadFile(path)
		return err == nil && string(data) == spkGolden(ip)
	}, 2*time.Second, 10*time.Millisecond, "announce file should hold exactly the golden bytes")
	assert.Eventually(t, func() bool {
		return slices.Contains(h.binder.ensuredList(), ip)
	}, 2*time.Second, 10*time.Millisecond, "binder should have ensured the ingress IP")
}

// A foreign-class Service is never announced here, even when its status
// holds an in-pool address: that allocation is another controller's, and
// announcing it would hijack the traffic.
func TestSpeakerSkipsForeignClassService(t *testing.T) {
	h := newSpkHarnessWithClass(t, "uk-node-1", "nylon")
	ip := mustAddr(t, "10.110.0.15")

	svc := spkService("default", "web", ip.String(), "")
	c := "other"
	svc.Spec.LoadBalancerClass = &c
	h.createService(svc)

	path := filepath.Join(h.dir, fileName("default", "web"))
	assert.Never(t, func() bool { return spkExists(path) }, time.Second, 10*time.Millisecond,
		"a foreign-class Service must never announce on this node")
	assert.Never(t, func() bool { return len(h.binder.ensuredList()) > 0 }, time.Second, 10*time.Millisecond,
		"the binder must never be called for a foreign-class Service")
}

// TestSpeakerAnnouncesAcrossPools: the speaker mirrors the allocator's
// output for any configured pool — two Services with manually set statuses,
// one per pool, both Cluster policy, each write their announce file with the
// golden bytes and both /32s get bound.
func TestSpeakerAnnouncesAcrossPools(t *testing.T) {
	h := newSpkHarness(t, "uk-node-1")
	shared := mustAddr(t, "10.110.0.10")
	private := mustAddr(t, "198.51.100.10")

	h.createService(spkService("default", "web", shared.String(), ""))
	h.createService(spkService("default", "mesh", private.String(), ""))

	for _, tc := range []struct {
		path string
		ip   netip.Addr
	}{
		{filepath.Join(h.dir, fileName("default", "web")), shared},
		{filepath.Join(h.dir, fileName("default", "mesh")), private},
	} {
		assert.Eventually(t, func() bool {
			data, err := os.ReadFile(tc.path)
			return err == nil && string(data) == spkGolden(tc.ip)
		}, 2*time.Second, 10*time.Millisecond, "announce file should hold exactly the golden bytes")
	}
	assert.Eventually(t, func() bool {
		ensured := h.binder.ensuredList()
		return slices.Contains(ensured, shared) && slices.Contains(ensured, private)
	}, 2*time.Second, 10*time.Millisecond, "binder should have ensured both pools' ingress IPs")
}

// TestSpeakerGCUnbindsForeignPoolAnnounce: an announce file orphaned by a
// removed Service whose address lives in the private pool is garbage-
// collected and its address unbound — any-pool fileAnnouncedIPs is what
// drives the unbind; scoped to the shared pool alone it would be skipped.
func TestSpeakerGCUnbindsForeignPoolAnnounce(t *testing.T) {
	h := newSpkHarness(t, "uk-node-1")
	ghost := mustAddr(t, "198.51.100.7")
	spkWriteAnnounceFile(t, h.dir, "lb-default-ghost.json", ghost)

	path := filepath.Join(h.dir, "lb-default-ghost.json")
	assert.Eventually(t, func() bool { return !spkExists(path) },
		2*time.Second, 10*time.Millisecond, "ghost announce should be garbage-collected")
	assert.Eventually(t, func() bool {
		return slices.Contains(h.binder.removedList(), ghost)
	}, 2*time.Second, 10*time.Millisecond, "withdrawn ghost announce should unbind its private-pool address")
}

// TestWithdrawDrainsBeforeUnbind: withdrawal removes the announce file
// immediately but keeps the address bound for the drain delay, so flows the
// mesh is still routing here survive until it converges on the withdrawal;
// neither the withdrawal path nor the drift sweep may unbind early.
func TestWithdrawDrainsBeforeUnbind(t *testing.T) {
	h := newSpkHarnessWithClient(t, fake.NewSimpleClientset(), "uk-node-1", "", 5*time.Second)
	ip := mustAddr(t, "10.110.0.17")

	h.createService(spkService("default", "drain", ip.String(), corev1.ServiceExternalTrafficPolicyLocal))
	ready := true
	h.createSlice(spkSlice("default", "drain-1", "drain", "uk-node-1", &ready))

	path := filepath.Join(h.dir, fileName("default", "drain"))
	assert.Eventually(t, func() bool { return spkExists(path) },
		2*time.Second, 10*time.Millisecond, "ready local endpoint should announce")
	assert.Eventually(t, func() bool { return h.binder.contains(ip) },
		2*time.Second, 10*time.Millisecond, "announce should bind the address")

	notReady := false
	h.updateSlice(spkSlice("default", "drain-1", "drain", "uk-node-1", &notReady))

	assert.Eventually(t, func() bool { return !spkExists(path) },
		2*time.Second, 10*time.Millisecond, "withdrawal should remove the announce file immediately")
	assert.Never(t, func() bool { return !h.binder.contains(ip) },
		500*time.Millisecond, 10*time.Millisecond, "address must stay bound for the drain delay")
	assert.Empty(t, h.binder.removedList(), "no unbind may happen before the drain expires")
}

// TestExpiredDrainUnbinds: once the drain deadline passes, the fallback sweep
// unbinds the address even though the announce file is long gone.
func TestExpiredDrainUnbinds(t *testing.T) {
	h := newSpkHarnessWithClient(t, fake.NewSimpleClientset(), "uk-node-1", "", 100*time.Millisecond)
	ip := mustAddr(t, "10.110.0.18")

	h.createService(spkService("default", "drain", ip.String(), corev1.ServiceExternalTrafficPolicyLocal))
	ready := true
	h.createSlice(spkSlice("default", "drain-1", "drain", "uk-node-1", &ready))

	path := filepath.Join(h.dir, fileName("default", "drain"))
	assert.Eventually(t, func() bool { return spkExists(path) },
		2*time.Second, 10*time.Millisecond, "ready local endpoint should announce")
	assert.Eventually(t, func() bool { return h.binder.contains(ip) },
		2*time.Second, 10*time.Millisecond, "announce should bind the address")

	notReady := false
	h.updateSlice(spkSlice("default", "drain-1", "drain", "uk-node-1", &notReady))

	assert.Eventually(t, func() bool { return !spkExists(path) },
		2*time.Second, 10*time.Millisecond, "withdrawal should remove the announce file")
	assert.Eventually(t, func() bool {
		return slices.Contains(h.binder.removedList(), ip)
	}, 2*time.Second, 10*time.Millisecond, "expired drain should unbind the address")
	assert.False(t, h.binder.contains(ip), "address should be gone after the drain expires")
}
