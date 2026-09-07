package main

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ctrlHarness wires a claim-all Controller (empty --lb-class) over pools;
// see ctrlHarnessWithClass.
func ctrlHarness(t *testing.T, pools *PoolSet) (*fake.Clientset, *record.FakeRecorder, func()) {
	t.Helper()
	return ctrlHarnessWithClass(t, pools, "")
}

// ctrlHarnessWithClass is ctrlHarness with --lb-class set to lbClass,
// scoping the controller to Services carrying that spec.loadBalancerClass.
func ctrlHarnessWithClass(t *testing.T, pools *PoolSet, lbClass string) (*fake.Clientset, *record.FakeRecorder, func()) {
	t.Helper()
	cs := fake.NewSimpleClientset()
	recorder := record.NewFakeRecorder(64)
	factory := informers.NewSharedInformerFactory(cs, 100*time.Millisecond)
	controller := NewController(ControllerOptions{
		Client:   cs,
		Factory:  factory,
		Pools:    pools,
		LBClass:  lbClass,
		Recorder: recorder,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	ctx, cancel := context.WithCancel(context.Background())
	factory.Start(ctx.Done())
	for typ, synced := range factory.WaitForCacheSync(ctx.Done()) {
		if !synced {
			cancel()
			t.Fatalf("informer %v failed to sync", typ)
		}
	}
	go func() { _ = controller.Run(ctx) }()
	t.Cleanup(cancel)
	return cs, recorder, cancel
}

// mustPoolSet parses specs of the form name=cidr into a PoolSet, failing
// the test on a bad spec. With no specs it defaults to a single pool named
// test spanning 192.0.2.0/29 (usable .1-.6).
func mustPoolSet(t *testing.T, specs ...string) *PoolSet {
	t.Helper()
	if len(specs) == 0 {
		specs = []string{"test=192.0.2.0/29"}
	}
	pools, err := ParsePoolSet(specs, nil)
	require.NoError(t, err)
	return pools
}

// ctrlDrainEvents collects whatever is currently buffered on the fake
// recorder's channel without blocking.
func ctrlDrainEvents(rec *record.FakeRecorder) []string {
	var events []string
	for {
		select {
		case ev := <-rec.Events:
			events = append(events, ev)
		default:
			return events
		}
	}
}

// ctrlAwaitEvent keeps draining until an event containing substr shows up.
// It returns every event seen so far. Polling happens in this goroutine on
// purpose: an assert.Eventually condition runs in a separate goroutine that
// may outlive the call and race the returned slice.
func ctrlAwaitEvent(t *testing.T, rec *record.FakeRecorder, substr string) []string {
	t.Helper()
	var events []string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		events = append(events, ctrlDrainEvents(rec)...)
		if slices.ContainsFunc(events, func(ev string) bool { return strings.Contains(ev, substr) }) {
			return events
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("waiting for event containing %q; events seen: %v", substr, events)
	return events
}

// ctrlService builds a minimal LoadBalancer Service in the default namespace;
// each mutate adjusts it in order (nil mutates are skipped).
func ctrlService(name string, mutate ...func(*corev1.Service)) *corev1.Service {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer},
	}
	for _, m := range mutate {
		if m != nil {
			m(svc)
		}
	}
	return svc
}

// ctrlWithClass returns a ctrlService mutate setting spec.loadBalancerClass.
func ctrlWithClass(class string) func(*corev1.Service) {
	return func(s *corev1.Service) {
		c := class
		s.Spec.LoadBalancerClass = &c
	}
}

// ctrlWithPool returns a ctrlService mutate selecting the named allocation
// pool via the nylon.io/lb-pool annotation.
func ctrlWithPool(name string) func(*corev1.Service) {
	return func(s *corev1.Service) {
		if s.Annotations == nil {
			s.Annotations = map[string]string{}
		}
		s.Annotations[PoolAnnotation] = name
	}
}

// ctrlStatusIngress returns the ingress IPs currently on the service status.
func ctrlStatusIngress(t *testing.T, cs *fake.Clientset, name string) []string {
	t.Helper()
	svc, err := cs.CoreV1().Services("default").Get(context.Background(), name, metav1.GetOptions{})
	require.NoError(t, err)
	return ingressIPStrings(svc.Status.LoadBalancer.Ingress)
}

// ctrlAwaitIngress waits until the service status carries exactly the given
// ingress IPs, in order.
func ctrlAwaitIngress(t *testing.T, cs *fake.Clientset, name string, want ...string) {
	t.Helper()
	assert.Eventually(t, func() bool {
		return slices.Equal(ctrlStatusIngress(t, cs, name), want)
	}, 5*time.Second, 50*time.Millisecond, "service %s ingress", name)
}

// A LoadBalancer Service is allocated the pool's lowest usable address: for
// 192.0.2.0/29 that is 192.0.2.1 (network .0 and broadcast .7 are skipped).
func TestControllerAllocatesLowestFreeIP(t *testing.T) {
	cs, _, _ := ctrlHarness(t, mustPoolSet(t))

	_, err := cs.CoreV1().Services("default").Create(
		context.Background(), ctrlService("web", ctrlWithPool("test")), metav1.CreateOptions{})
	require.NoError(t, err)

	ctrlAwaitIngress(t, cs, "web", "192.0.2.1")
}

// A second Service gets the next free address and the first keeps its own.
func TestControllerAllocatesDistinctIPs(t *testing.T) {
	cs, _, _ := ctrlHarness(t, mustPoolSet(t))

	_, err := cs.CoreV1().Services("default").Create(
		context.Background(), ctrlService("one", ctrlWithPool("test")), metav1.CreateOptions{})
	require.NoError(t, err)
	ctrlAwaitIngress(t, cs, "one", "192.0.2.1")

	_, err = cs.CoreV1().Services("default").Create(
		context.Background(), ctrlService("two", ctrlWithPool("test")), metav1.CreateOptions{})
	require.NoError(t, err)
	ctrlAwaitIngress(t, cs, "two", "192.0.2.2")

	assert.Equal(t, []string{"192.0.2.1"}, ctrlStatusIngress(t, cs, "one"),
		"first service must keep its allocation")
}

// A spec.loadBalancerIP that is in-pool and free is honored verbatim.
func TestControllerHonorsRequestedIP(t *testing.T) {
	cs, rec, _ := ctrlHarness(t, mustPoolSet(t))

	_, err := cs.CoreV1().Services("default").Create(
		context.Background(),
		ctrlService("req", ctrlWithPool("test"), func(s *corev1.Service) { s.Spec.LoadBalancerIP = "192.0.2.5" }),
		metav1.CreateOptions{})
	require.NoError(t, err)

	ctrlAwaitIngress(t, cs, "req", "192.0.2.5")

	// Give any straggler event time to land, then confirm the request was
	// honored silently: no warning may exist for a valid request.
	time.Sleep(200 * time.Millisecond)
	events := ctrlDrainEvents(rec)
	assert.False(t, slices.ContainsFunc(events, func(ev string) bool { return strings.Contains(ev, "BadLoadBalancerIP") }),
		"valid request must not warn, got events: %v", events)
}

// A spec.loadBalancerIP outside the pool draws a BadLoadBalancerIP warning and
// the service still ends up with a pool-allocated address.
func TestControllerWarnsOnOutOfPoolRequestedIP(t *testing.T) {
	cs, rec, _ := ctrlHarness(t, mustPoolSet(t))

	_, err := cs.CoreV1().Services("default").Create(
		context.Background(),
		ctrlService("ext", ctrlWithPool("test"), func(s *corev1.Service) { s.Spec.LoadBalancerIP = "10.9.9.9" }),
		metav1.CreateOptions{})
	require.NoError(t, err)

	ctrlAwaitIngress(t, cs, "ext", "192.0.2.1")
	ctrlAwaitEvent(t, rec, "BadLoadBalancerIP")
}

// A spec.loadBalancerIP already held by another service's ingress is refused
// with a BadLoadBalancerIP warning and a different pool address is allocated.
func TestControllerWarnsOnTakenRequestedIP(t *testing.T) {
	cs, rec, _ := ctrlHarness(t, mustPoolSet(t))

	_, err := cs.CoreV1().Services("default").Create(
		context.Background(), ctrlService("one", ctrlWithPool("test")), metav1.CreateOptions{})
	require.NoError(t, err)
	ctrlAwaitIngress(t, cs, "one", "192.0.2.1")

	_, err = cs.CoreV1().Services("default").Create(
		context.Background(),
		ctrlService("two", ctrlWithPool("test"), func(s *corev1.Service) { s.Spec.LoadBalancerIP = "192.0.2.1" }),
		metav1.CreateOptions{})
	require.NoError(t, err)

	ctrlAwaitIngress(t, cs, "two", "192.0.2.2")
	ctrlAwaitEvent(t, rec, "BadLoadBalancerIP")
	assert.Equal(t, []string{"192.0.2.1"}, ctrlStatusIngress(t, cs, "one"),
		"holder of the requested IP must be untouched")
}

// Flipping a Service to ClusterIP releases the address: the ingress is cleared
// and a Released event is emitted.
func TestControllerReleasesIngressOnTypeChange(t *testing.T) {
	cs, rec, _ := ctrlHarness(t, mustPoolSet(t))

	_, err := cs.CoreV1().Services("default").Create(
		context.Background(), ctrlService("flip", ctrlWithPool("test")), metav1.CreateOptions{})
	require.NoError(t, err)
	ctrlAwaitIngress(t, cs, "flip", "192.0.2.1")

	svc, err := cs.CoreV1().Services("default").Get(context.Background(), "flip", metav1.GetOptions{})
	require.NoError(t, err)
	updated := svc.DeepCopy()
	updated.Spec.Type = corev1.ServiceTypeClusterIP
	_, err = cs.CoreV1().Services("default").Update(context.Background(), updated, metav1.UpdateOptions{})
	require.NoError(t, err)

	ctrlAwaitIngress(t, cs, "flip")
	ctrlAwaitEvent(t, rec, "Released")
}

// With a /30 pool (usable .1 and .2) two services exhaust it; a third never
// gets an ingress and surfaces the AllocationFailed warning instead.
func TestControllerBacksOffWhenPoolExhausted(t *testing.T) {
	cs, rec, _ := ctrlHarness(t, mustPoolSet(t, "test=192.0.2.0/30"))

	for _, name := range []string{"a", "b"} {
		_, err := cs.CoreV1().Services("default").Create(
			context.Background(), ctrlService(name, ctrlWithPool("test")), metav1.CreateOptions{})
		require.NoError(t, err)
	}
	ctrlAwaitIngress(t, cs, "a", "192.0.2.1")
	ctrlAwaitIngress(t, cs, "b", "192.0.2.2")

	_, err := cs.CoreV1().Services("default").Create(
		context.Background(), ctrlService("c", ctrlWithPool("test")), metav1.CreateOptions{})
	require.NoError(t, err)

	// Absence cannot be proven, only sampled: hold for a full second and
	// fail if an ingress ever appears.
	assert.Never(t, func() bool {
		return len(ctrlStatusIngress(t, cs, "c")) > 0
	}, time.Second, 50*time.Millisecond, "exhausted pool must not allocate")
	ctrlAwaitEvent(t, rec, "AllocationFailed")
}

// A service seeded with a foreign (out-of-pool) ingress gets it replaced by a
// pool address plus an Allocated event.
func TestControllerReplacesForeignIngress(t *testing.T) {
	cs, rec, _ := ctrlHarness(t, mustPoolSet(t))

	seeded := ctrlService("stale", ctrlWithPool("test"))
	seeded.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: "203.0.113.9"}}
	_, err := cs.CoreV1().Services("default").Create(context.Background(), seeded, metav1.CreateOptions{})
	require.NoError(t, err)

	ctrlAwaitIngress(t, cs, "stale", "192.0.2.1")
	ctrlAwaitEvent(t, rec, "Allocated")
}

// A class-scoped controller allocates for a Service carrying exactly its
// loadBalancerClass.
func TestControllerAllocatesMatchingClassService(t *testing.T) {
	cs, rec, _ := ctrlHarnessWithClass(t, mustPoolSet(t), "nylon")

	_, err := cs.CoreV1().Services("default").Create(
		context.Background(), ctrlService("web", ctrlWithClass("nylon"), ctrlWithPool("test")), metav1.CreateOptions{})
	require.NoError(t, err)

	ctrlAwaitIngress(t, cs, "web", "192.0.2.1")
	ctrlAwaitEvent(t, rec, "Allocated")
}

// A Service carrying a foreign loadBalancerClass is never given an ingress
// and produces no events: it belongs to another controller.
func TestControllerIgnoresForeignClassService(t *testing.T) {
	cs, rec, _ := ctrlHarnessWithClass(t, mustPoolSet(t), "nylon")

	_, err := cs.CoreV1().Services("default").Create(
		context.Background(), ctrlService("foreign", ctrlWithClass("other"), ctrlWithPool("test")), metav1.CreateOptions{})
	require.NoError(t, err)

	assert.Never(t, func() bool {
		return len(ctrlStatusIngress(t, cs, "foreign")) > 0
	}, time.Second, 50*time.Millisecond, "foreign-class Service must never get an ingress")
	assert.Empty(t, ctrlDrainEvents(rec), "foreign-class Service must produce no events")
}

// While --lb-class is set, an unclassed Service stays pending for another
// controller: strict match, not fallback-to-unclaimed.
func TestControllerIgnoresUnclassedServiceWhenClassSet(t *testing.T) {
	cs, _, _ := ctrlHarnessWithClass(t, mustPoolSet(t), "nylon")

	_, err := cs.CoreV1().Services("default").Create(
		context.Background(), ctrlService("unclassed", ctrlWithPool("test")), metav1.CreateOptions{})
	require.NoError(t, err)

	assert.Never(t, func() bool {
		return len(ctrlStatusIngress(t, cs, "unclassed")) > 0
	}, time.Second, 50*time.Millisecond, "unclassed Service must stay pending")
}

// Flipping an owned Service's class away hands it over: the status we wrote
// stays exactly as it was — never re-written, never wiped. The new class's
// controller owns the Service now; never clear an address on a Service this
// controller no longer owns.
func TestControllerHandsOffOnClassChangeAway(t *testing.T) {
	cs, rec, _ := ctrlHarnessWithClass(t, mustPoolSet(t), "nylon")

	_, err := cs.CoreV1().Services("default").Create(
		context.Background(), ctrlService("flip", ctrlWithClass("nylon"), ctrlWithPool("test")), metav1.CreateOptions{})
	require.NoError(t, err)
	ctrlAwaitIngress(t, cs, "flip", "192.0.2.1")

	svc, err := cs.CoreV1().Services("default").Get(context.Background(), "flip", metav1.GetOptions{})
	require.NoError(t, err)
	updated := svc.DeepCopy()
	c := "other"
	updated.Spec.LoadBalancerClass = &c
	_, err = cs.CoreV1().Services("default").Update(context.Background(), updated, metav1.UpdateOptions{})
	require.NoError(t, err)

	assert.Never(t, func() bool {
		return !slices.Equal(ctrlStatusIngress(t, cs, "flip"), []string{"192.0.2.1"})
	}, time.Second, 50*time.Millisecond, "handed-off Service status must stay untouched")
	assert.False(t, slices.ContainsFunc(ctrlDrainEvents(rec), func(ev string) bool {
		return strings.Contains(ev, "Released")
	}), "handing off must not release the address")
}

// A foreign-class Service's status is untouchable even when it is out of
// pool: another controller wrote it, and nylon-lb never clears addresses it
// did not allocate.
func TestControllerPreservesForeignIngressOnNotOwned(t *testing.T) {
	cs, _, _ := ctrlHarnessWithClass(t, mustPoolSet(t), "nylon")

	seeded := ctrlService("foreign", ctrlWithClass("other"), ctrlWithPool("test"))
	seeded.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: "203.0.113.9"}}
	_, err := cs.CoreV1().Services("default").Create(context.Background(), seeded, metav1.CreateOptions{})
	require.NoError(t, err)

	assert.Never(t, func() bool {
		return !slices.Equal(ctrlStatusIngress(t, cs, "foreign"), []string{"203.0.113.9"})
	}, time.Second, 50*time.Millisecond, "foreign-class status must stay untouched")
}

// The taken set is class-agnostic: an in-pool address held by a
// foreign-class Service still counts as taken, so it is never handed to an
// owned Service — overlapping pools must never double-assign.
func TestControllerForeignInPoolIngressCountsAsTaken(t *testing.T) {
	// /30: usable .1 and .2
	cs, _, _ := ctrlHarnessWithClass(t, mustPoolSet(t, "test=192.0.2.0/30"), "nylon")

	foreign := ctrlService("foreign", ctrlWithClass("other"), ctrlWithPool("test"))
	foreign.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: "192.0.2.1"}}
	_, err := cs.CoreV1().Services("default").Create(context.Background(), foreign, metav1.CreateOptions{})
	require.NoError(t, err)

	_, err = cs.CoreV1().Services("default").Create(
		context.Background(), ctrlService("web", ctrlWithClass("nylon"), ctrlWithPool("test")), metav1.CreateOptions{})
	require.NoError(t, err)

	ctrlAwaitIngress(t, cs, "web", "192.0.2.2")
	assert.Equal(t, []string{"192.0.2.1"}, ctrlStatusIngress(t, cs, "foreign"),
		"the foreign-class holder keeps its address (hands off)")
}

// A LoadBalancer Service without the pool annotation fails closed: a
// MissingPoolAnnotation warning is emitted and the status never gains an
// ingress.
func TestControllerMissingPoolAnnotationFailClosed(t *testing.T) {
	cs, rec, _ := ctrlHarness(t, mustPoolSet(t))

	_, err := cs.CoreV1().Services("default").Create(
		context.Background(), ctrlService("bare"), metav1.CreateOptions{})
	require.NoError(t, err)

	ctrlAwaitEvent(t, rec, "MissingPoolAnnotation")
	assert.Never(t, func() bool {
		return len(ctrlStatusIngress(t, cs, "bare")) > 0
	}, time.Second, 50*time.Millisecond, "unannotated Service must stay pending")
}

// An annotation naming a pool that does not exist fails closed the same
// way: an UnknownPool warning and no ingress.
func TestControllerUnknownPoolFailClosed(t *testing.T) {
	cs, rec, _ := ctrlHarness(t, mustPoolSet(t))

	_, err := cs.CoreV1().Services("default").Create(
		context.Background(), ctrlService("lost", ctrlWithPool("nope")), metav1.CreateOptions{})
	require.NoError(t, err)

	ctrlAwaitEvent(t, rec, "UnknownPool")
	assert.Never(t, func() bool {
		return len(ctrlStatusIngress(t, cs, "lost")) > 0
	}, time.Second, 50*time.Millisecond, "unknown-pool Service must stay pending")
}

// Stripping the pool annotation from an allocated Service releases the
// address: the ingress is emptied and a Released event is emitted.
func TestControllerMissingAnnotationReleasesStaleIngress(t *testing.T) {
	cs, rec, _ := ctrlHarness(t, mustPoolSet(t))

	_, err := cs.CoreV1().Services("default").Create(
		context.Background(), ctrlService("web", ctrlWithPool("test")), metav1.CreateOptions{})
	require.NoError(t, err)
	ctrlAwaitIngress(t, cs, "web", "192.0.2.1")

	svc, err := cs.CoreV1().Services("default").Get(context.Background(), "web", metav1.GetOptions{})
	require.NoError(t, err)
	updated := svc.DeepCopy()
	delete(updated.Annotations, PoolAnnotation)
	_, err = cs.CoreV1().Services("default").Update(context.Background(), updated, metav1.UpdateOptions{})
	require.NoError(t, err)

	ctrlAwaitIngress(t, cs, "web")
	ctrlAwaitEvent(t, rec, "Released")
}

// Each Service allocates from the pool its annotation selects: addresses
// never cross pools and stay lowest-free within a pool.
func TestControllerPoolSelection(t *testing.T) {
	cs, _, _ := ctrlHarness(t, mustPoolSet(t, "shared=192.0.2.0/29", "private=198.51.100.0/29"))

	_, err := cs.CoreV1().Services("default").Create(
		context.Background(), ctrlService("a", ctrlWithPool("shared")), metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = cs.CoreV1().Services("default").Create(
		context.Background(), ctrlService("b", ctrlWithPool("private")), metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = cs.CoreV1().Services("default").Create(
		context.Background(), ctrlService("c", ctrlWithPool("shared")), metav1.CreateOptions{})
	require.NoError(t, err)

	ctrlAwaitIngress(t, cs, "a", "192.0.2.1")
	ctrlAwaitIngress(t, cs, "b", "198.51.100.1")
	ctrlAwaitIngress(t, cs, "c", "192.0.2.2")
}

// Re-pointing the annotation at another pool reassigns from the newly
// selected pool, and the freed address returns to the old one.
func TestControllerAnnotationFlipReassigns(t *testing.T) {
	cs, _, _ := ctrlHarness(t, mustPoolSet(t, "shared=192.0.2.0/29", "private=198.51.100.0/29"))

	_, err := cs.CoreV1().Services("default").Create(
		context.Background(), ctrlService("web", ctrlWithPool("shared")), metav1.CreateOptions{})
	require.NoError(t, err)
	ctrlAwaitIngress(t, cs, "web", "192.0.2.1")

	svc, err := cs.CoreV1().Services("default").Get(context.Background(), "web", metav1.GetOptions{})
	require.NoError(t, err)
	updated := svc.DeepCopy()
	updated.Annotations[PoolAnnotation] = "private"
	_, err = cs.CoreV1().Services("default").Update(context.Background(), updated, metav1.UpdateOptions{})
	require.NoError(t, err)

	ctrlAwaitIngress(t, cs, "web", "198.51.100.1")

	// The freed shared address is immediately reusable by the next shared
	// Service.
	_, err = cs.CoreV1().Services("default").Create(
		context.Background(), ctrlService("next", ctrlWithPool("shared")), metav1.CreateOptions{})
	require.NoError(t, err)
	ctrlAwaitIngress(t, cs, "next", "192.0.2.1")
}

// A spec.loadBalancerIP inside a configured pool but outside the SELECTED
// pool is refused with a BadLoadBalancerIP warning; the Service still
// allocates from the pool its annotation selects.
func TestControllerLoadBalancerIPOutsideSelectedPool(t *testing.T) {
	cs, rec, _ := ctrlHarness(t, mustPoolSet(t, "shared=192.0.2.0/29", "private=198.51.100.0/29"))

	_, err := cs.CoreV1().Services("default").Create(
		context.Background(),
		ctrlService("web", ctrlWithPool("private"), func(s *corev1.Service) { s.Spec.LoadBalancerIP = "192.0.2.3" }),
		metav1.CreateOptions{})
	require.NoError(t, err)

	ctrlAwaitEvent(t, rec, "BadLoadBalancerIP")
	ctrlAwaitIngress(t, cs, "web", "198.51.100.1")
}
