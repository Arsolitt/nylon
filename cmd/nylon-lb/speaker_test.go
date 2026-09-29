package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
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

// spkObserved retries mutate until every harness's informers report observed,
// then returns. The fake clientset delivers watch events only to watchers
// that were registered when the mutation happened, so a mutation racing
// informer startup is otherwise lost for the rest of the test; retrying it
// closes that window. Multi-node simulations pass every harness sharing the
// clientset so all of them observe the change.
func spkObserved(t *testing.T, what string, mutate func() error, hs []*spkHarness, observed func(*spkHarness) bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := mutate()
		if err != nil && !apierrors.IsAlreadyExists(err) && !apierrors.IsNotFound(err) {
			t.Fatalf("mutating %s: %v", what, err)
		}
		if spkWaitObserved(hs, observed) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("informer caches never observed %s", what)
		}
	}
}

// spkWaitObserved polls observed for every harness until all report true or
// the window closes.
func spkWaitObserved(hs []*spkHarness, observed func(*spkHarness) bool) bool {
	deadline := time.Now().Add(200 * time.Millisecond)
	for {
		all := true
		for _, h := range hs {
			if !observed(h) {
				all = false
				break
			}
		}
		if all {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func spkServiceObserved(svc *corev1.Service) func(*spkHarness) bool {
	return func(h *spkHarness) bool {
		got, err := h.speaker.svcLister.Services(svc.Namespace).Get(svc.Name)
		return err == nil && reflect.DeepEqual(got.Spec, svc.Spec) &&
			reflect.DeepEqual(got.Annotations, svc.Annotations) &&
			reflect.DeepEqual(got.Status, svc.Status)
	}
}

func spkSliceObserved(slice *discoveryv1.EndpointSlice) func(*spkHarness) bool {
	return func(h *spkHarness) bool {
		got, err := h.speaker.sliceLister.EndpointSlices(slice.Namespace).Get(slice.Name)
		return err == nil && reflect.DeepEqual(got.Endpoints, slice.Endpoints) &&
			reflect.DeepEqual(got.Labels, slice.Labels)
	}
}

func spkSliceGone(ns, name string) func(*spkHarness) bool {
	return func(h *spkHarness) bool {
		_, err := h.speaker.sliceLister.EndpointSlices(ns).Get(name)
		return apierrors.IsNotFound(err)
	}
}

// spkCreateService publishes svc and waits until every harness's Service
// informer observes it. A retry re-sends the object as an update: create is
// idempotent (AlreadyExists) but emits no event, so only an update can reach
// an informer whose watcher registration raced the create.
func spkCreateService(t *testing.T, hs []*spkHarness, svc *corev1.Service) {
	t.Helper()
	spkObserved(t, "service "+svc.Namespace+"/"+svc.Name, func() error {
		client := hs[0].client.CoreV1().Services(svc.Namespace)
		_, err := client.Create(context.Background(), svc, metav1.CreateOptions{})
		if !apierrors.IsAlreadyExists(err) {
			return err
		}
		_, err = client.Update(context.Background(), svc, metav1.UpdateOptions{})
		return err
	}, hs, spkServiceObserved(svc))
}

// spkCreateSlice publishes slice and waits until every harness's EndpointSlice
// informer observes it; see spkCreateService for the update fallback.
func spkCreateSlice(t *testing.T, hs []*spkHarness, slice *discoveryv1.EndpointSlice) {
	t.Helper()
	spkObserved(t, "slice "+slice.Namespace+"/"+slice.Name, func() error {
		client := hs[0].client.DiscoveryV1().EndpointSlices(slice.Namespace)
		_, err := client.Create(context.Background(), slice, metav1.CreateOptions{})
		if !apierrors.IsAlreadyExists(err) {
			return err
		}
		_, err = client.Update(context.Background(), slice, metav1.UpdateOptions{})
		return err
	}, hs, spkSliceObserved(slice))
}

// spkUpdateSlice updates slice and waits until every harness's EndpointSlice
// informer observes the new content.
func spkUpdateSlice(t *testing.T, hs []*spkHarness, slice *discoveryv1.EndpointSlice) {
	t.Helper()
	spkObserved(t, "slice "+slice.Namespace+"/"+slice.Name, func() error {
		_, err := hs[0].client.DiscoveryV1().EndpointSlices(slice.Namespace).Update(context.Background(), slice, metav1.UpdateOptions{})
		return err
	}, hs, spkSliceObserved(slice))
}

// spkDeleteSlice deletes ns/name and waits until every harness's EndpointSlice
// informer observes the deletion.
func spkDeleteSlice(t *testing.T, hs []*spkHarness, ns, name string) {
	t.Helper()
	spkObserved(t, "slice "+ns+"/"+name, func() error {
		return hs[0].client.DiscoveryV1().EndpointSlices(ns).Delete(context.Background(), name, metav1.DeleteOptions{})
	}, hs, spkSliceGone(ns, name))
}

func (h *spkHarness) createService(svc *corev1.Service) {
	h.t.Helper()
	spkCreateService(h.t, []*spkHarness{h}, svc)
}

func (h *spkHarness) createSlice(slice *discoveryv1.EndpointSlice) {
	h.t.Helper()
	spkCreateSlice(h.t, []*spkHarness{h}, slice)
}

func (h *spkHarness) updateSlice(slice *discoveryv1.EndpointSlice) {
	h.t.Helper()
	spkUpdateSlice(h.t, []*spkHarness{h}, slice)
}

func (h *spkHarness) deleteSlice(ns, name string) {
	h.t.Helper()
	spkDeleteSlice(h.t, []*spkHarness{h}, ns, name)
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

// spkSliceForNodes builds an EndpointSlice labelled for svcName with one
// ready endpoint per node, in order. A speaker observes endpoints a slice at
// a time, so nodes sharing one object always agree on the eligible set —
// separate slices would let a speaker briefly see a subset of the nodes.
func spkSliceForNodes(ns, name, svcName string, nodes ...string) *discoveryv1.EndpointSlice {
	ready := true
	slice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      name,
			Labels:    map[string]string{discoveryv1.LabelServiceName: svcName},
		},
	}
	for _, node := range nodes {
		slice.Endpoints = append(slice.Endpoints, discoveryv1.Endpoint{
			NodeName:   &node,
			Conditions: discoveryv1.EndpointConditions{Ready: &ready},
		})
	}
	return slice
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

// spkSingleMode marks a Service as nylon.io/announce: single.
func spkSingleMode(svc *corev1.Service) *corev1.Service {
	if svc.Annotations == nil {
		svc.Annotations = map[string]string{}
	}
	svc.Annotations[AnnounceAnnotation] = announceModeSingle
	return svc
}

// TestSingleAnnounceElectsOneNode: with a ready endpoint on both simulated
// nodes, exactly one of them announces the single-mode Service — the one the
// deterministic election picks; the other neither writes the file nor binds
// the address.
func TestSingleAnnounceElectsOneNode(t *testing.T) {
	client := fake.NewSimpleClientset()
	a := newSpkHarnessWithClient(t, client, "node-a", "", 0)
	b := newSpkHarnessWithClient(t, client, "node-b", "", 0)
	ip := mustAddr(t, "10.110.0.20")

	spkCreateService(t, []*spkHarness{a, b},
		spkSingleMode(spkService("default", "svc", ip.String(), corev1.ServiceExternalTrafficPolicyLocal)))
	spkCreateSlice(t, []*spkHarness{a, b}, spkSliceForNodes("default", "svc-1", "svc", "node-a", "node-b"))

	winner, loser := a, b
	if singleAnnouncer("default/svc", []string{"node-a", "node-b"}) == "node-b" {
		winner, loser = b, a
	}
	path := filepath.Join(winner.dir, fileName("default", "svc"))
	assert.Eventually(t, func() bool { return spkExists(path) },
		2*time.Second, 10*time.Millisecond, "the elected node should announce")
	assert.Never(t, func() bool { return spkExists(filepath.Join(loser.dir, fileName("default", "svc"))) },
		time.Second, 10*time.Millisecond, "the losing node must never announce")
	assert.Never(t, func() bool { return len(loser.binder.ensuredList()) > 0 },
		time.Second, 10*time.Millisecond, "the losing node must never bind the address")
}

// TestSingleAnnounceRequiresLocalPolicy: single mode with the Cluster policy
// is a misconfiguration; the Service falls back to anycast and every node
// announces it.
func TestSingleAnnounceRequiresLocalPolicy(t *testing.T) {
	client := fake.NewSimpleClientset()
	a := newSpkHarnessWithClient(t, client, "node-a", "", 0)
	b := newSpkHarnessWithClient(t, client, "node-b", "", 0)
	ip := mustAddr(t, "10.110.0.21")

	spkCreateService(t, []*spkHarness{a, b}, spkSingleMode(spkService("default", "svc", ip.String(), "")))
	ready := true
	spkCreateSlice(t, []*spkHarness{a, b}, spkSlice("default", "svc-1", "svc", "node-a", &ready))

	for _, h := range []*spkHarness{a, b} {
		path := filepath.Join(h.dir, fileName("default", "svc"))
		assert.Eventually(t, func() bool { return spkExists(path) },
			2*time.Second, 10*time.Millisecond, "Cluster policy should announce anycast despite single mode")
	}
}

// TestSingleAnnounceUnknownValueIsAnycast: an unrecognised announce mode must
// not stop the announce — it degrades to anycast.
func TestSingleAnnounceUnknownValueIsAnycast(t *testing.T) {
	h := newSpkHarness(t, "node-a")
	ip := mustAddr(t, "10.110.0.22")

	svc := spkService("default", "svc", ip.String(), corev1.ServiceExternalTrafficPolicyLocal)
	svc.Annotations = map[string]string{AnnounceAnnotation: "elect"}
	h.createService(svc)
	ready := true
	h.createSlice(spkSlice("default", "svc-1", "svc", "node-a", &ready))

	path := filepath.Join(h.dir, fileName("default", "svc"))
	assert.Eventually(t, func() bool { return spkExists(path) },
		2*time.Second, 10*time.Millisecond, "an unknown mode should announce anycast")
	assert.Eventually(t, func() bool { return h.binder.contains(ip) },
		2*time.Second, 10*time.Millisecond, "an unknown mode should still bind the address")
}

// TestSingleAnnounceWithdrawsWhenAPIStale: the elected announcer withdraws its
// single-mode announce once its own direct API view goes stale — a bounded
// black-hole beats two owners for a stateful path.
func TestSingleAnnounceWithdrawsWhenAPIStale(t *testing.T) {
	h := newSpkHarness(t, "node-a")
	ip := mustAddr(t, "10.110.0.23")

	h.createService(spkSingleMode(spkService("default", "svc", ip.String(), corev1.ServiceExternalTrafficPolicyLocal)))
	ready := true
	h.createSlice(spkSlice("default", "svc-1", "svc", "node-a", &ready))

	path := filepath.Join(h.dir, fileName("default", "svc"))
	assert.Eventually(t, func() bool { return spkExists(path) },
		2*time.Second, 10*time.Millisecond, "the only eligible node should announce")
	assert.Eventually(t, func() bool { return h.binder.contains(ip) },
		2*time.Second, 10*time.Millisecond, "the announce should bind the address")

	h.speaker.lastAPIOK.Store(time.Now().Add(-time.Hour).UnixNano())
	h.speaker.kick()

	assert.Eventually(t, func() bool { return !spkExists(path) },
		2*time.Second, 10*time.Millisecond, "a stale API view should withdraw the announce")
	assert.Eventually(t, func() bool { return !h.binder.contains(ip) },
		2*time.Second, 10*time.Millisecond, "withdrawal should unbind the address")
}

// TestSingleAnnounceFollowsEndpointHandover: the election follows the ready
// endpoints — when the only ready endpoint moves from one node to the other,
// the announce moves with it.
func TestSingleAnnounceFollowsEndpointHandover(t *testing.T) {
	client := fake.NewSimpleClientset()
	a := newSpkHarnessWithClient(t, client, "node-a", "", 0)
	b := newSpkHarnessWithClient(t, client, "node-b", "", 0)
	ip := mustAddr(t, "10.110.0.24")

	spkCreateService(t, []*spkHarness{a, b},
		spkSingleMode(spkService("default", "svc", ip.String(), corev1.ServiceExternalTrafficPolicyLocal)))
	ready := true
	spkCreateSlice(t, []*spkHarness{a, b}, spkSlice("default", "svc-1", "svc", "node-b", &ready))

	bPath := filepath.Join(b.dir, fileName("default", "svc"))
	aPath := filepath.Join(a.dir, fileName("default", "svc"))
	assert.Eventually(t, func() bool { return spkExists(bPath) },
		2*time.Second, 10*time.Millisecond, "the node holding the only ready endpoint should announce")

	spkUpdateSlice(t, []*spkHarness{a, b}, spkSlice("default", "svc-1", "svc", "node-a", &ready))

	assert.Eventually(t, func() bool { return spkExists(aPath) },
		2*time.Second, 10*time.Millisecond, "handing the endpoint over should move the announce")
	assert.Eventually(t, func() bool { return !spkExists(bPath) },
		2*time.Second, 10*time.Millisecond, "the previous announcer should withdraw")
}

// TestProbeAPIRecordsFailureAndSuccess: a failed direct API read leaves the
// recorded liveness untouched; a later success advances it.
func TestProbeAPIRecordsFailureAndSuccess(t *testing.T) {
	h := newSpkHarness(t, "node-a")
	var fail atomic.Bool
	fail.Store(true)
	h.client.PrependReactor("list", "endpointslices", func(k8stesting.Action) (bool, runtime.Object, error) {
		if fail.Load() {
			return true, nil, errors.New("api down")
		}
		return false, nil, nil
	})

	stale := time.Now().Add(-time.Minute).UnixNano()
	h.speaker.lastAPIOK.Store(stale)
	h.speaker.probeAPI(context.Background())
	assert.Equal(t, stale, h.speaker.lastAPIOK.Load(), "a failed probe must not renew the view")

	fail.Store(false)
	h.speaker.probeAPI(context.Background())
	assert.Greater(t, h.speaker.lastAPIOK.Load(), stale, "a successful probe should renew the view")
}
