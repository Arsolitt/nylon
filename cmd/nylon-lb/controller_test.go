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

// ctrlHarness wires a claim-all Controller (empty --lb-class); see
// ctrlHarnessWithClass.
func ctrlHarness(t *testing.T, pool *Pool) (*fake.Clientset, *record.FakeRecorder, func()) {
	t.Helper()
	return ctrlHarnessWithClass(t, pool, "")
}

// ctrlHarnessWithClass is ctrlHarness with --lb-class set to lbClass,
// scoping the controller to Services carrying that spec.loadBalancerClass.
func ctrlHarnessWithClass(t *testing.T, pool *Pool, lbClass string) (*fake.Clientset, *record.FakeRecorder, func()) {
	t.Helper()
	cs := fake.NewSimpleClientset()
	recorder := record.NewFakeRecorder(64)
	factory := informers.NewSharedInformerFactory(cs, 100*time.Millisecond)
	controller := NewController(ControllerOptions{
		Client:   cs,
		Factory:  factory,
		Pool:     pool,
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
// mutate adjusts it per case.
func ctrlService(name string, mutate func(*corev1.Service)) *corev1.Service {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer},
	}
	if mutate != nil {
		mutate(svc)
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
	pool, err := ParsePool("192.0.2.0/29", nil)
	require.NoError(t, err)
	cs, _, _ := ctrlHarness(t, pool)

	_, err = cs.CoreV1().Services("default").Create(
		context.Background(), ctrlService("web", nil), metav1.CreateOptions{})
	require.NoError(t, err)

	ctrlAwaitIngress(t, cs, "web", "192.0.2.1")
}

// A second Service gets the next free address and the first keeps its own.
func TestControllerAllocatesDistinctIPs(t *testing.T) {
	pool, err := ParsePool("192.0.2.0/29", nil)
	require.NoError(t, err)
	cs, _, _ := ctrlHarness(t, pool)

	_, err = cs.CoreV1().Services("default").Create(
		context.Background(), ctrlService("one", nil), metav1.CreateOptions{})
	require.NoError(t, err)
	ctrlAwaitIngress(t, cs, "one", "192.0.2.1")

	_, err = cs.CoreV1().Services("default").Create(
		context.Background(), ctrlService("two", nil), metav1.CreateOptions{})
	require.NoError(t, err)
	ctrlAwaitIngress(t, cs, "two", "192.0.2.2")

	assert.Equal(t, []string{"192.0.2.1"}, ctrlStatusIngress(t, cs, "one"),
		"first service must keep its allocation")
}

// A spec.loadBalancerIP that is in-pool and free is honored verbatim.
func TestControllerHonorsRequestedIP(t *testing.T) {
	pool, err := ParsePool("192.0.2.0/29", nil)
	require.NoError(t, err)
	cs, rec, _ := ctrlHarness(t, pool)

	_, err = cs.CoreV1().Services("default").Create(
		context.Background(),
		ctrlService("req", func(s *corev1.Service) { s.Spec.LoadBalancerIP = "192.0.2.5" }),
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
	pool, err := ParsePool("192.0.2.0/29", nil)
	require.NoError(t, err)
	cs, rec, _ := ctrlHarness(t, pool)

	_, err = cs.CoreV1().Services("default").Create(
		context.Background(),
		ctrlService("ext", func(s *corev1.Service) { s.Spec.LoadBalancerIP = "10.9.9.9" }),
		metav1.CreateOptions{})
	require.NoError(t, err)

	ctrlAwaitIngress(t, cs, "ext", "192.0.2.1")
	ctrlAwaitEvent(t, rec, "BadLoadBalancerIP")
}

// A spec.loadBalancerIP already held by another service's ingress is refused
// with a BadLoadBalancerIP warning and a different pool address is allocated.
func TestControllerWarnsOnTakenRequestedIP(t *testing.T) {
	pool, err := ParsePool("192.0.2.0/29", nil)
	require.NoError(t, err)
	cs, rec, _ := ctrlHarness(t, pool)

	_, err = cs.CoreV1().Services("default").Create(
		context.Background(), ctrlService("one", nil), metav1.CreateOptions{})
	require.NoError(t, err)
	ctrlAwaitIngress(t, cs, "one", "192.0.2.1")

	_, err = cs.CoreV1().Services("default").Create(
		context.Background(),
		ctrlService("two", func(s *corev1.Service) { s.Spec.LoadBalancerIP = "192.0.2.1" }),
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
	pool, err := ParsePool("192.0.2.0/29", nil)
	require.NoError(t, err)
	cs, rec, _ := ctrlHarness(t, pool)

	_, err = cs.CoreV1().Services("default").Create(
		context.Background(), ctrlService("flip", nil), metav1.CreateOptions{})
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
	pool, err := ParsePool("192.0.2.0/30", nil)
	require.NoError(t, err)
	cs, rec, _ := ctrlHarness(t, pool)

	for _, name := range []string{"a", "b"} {
		_, err = cs.CoreV1().Services("default").Create(
			context.Background(), ctrlService(name, nil), metav1.CreateOptions{})
		require.NoError(t, err)
	}
	ctrlAwaitIngress(t, cs, "a", "192.0.2.1")
	ctrlAwaitIngress(t, cs, "b", "192.0.2.2")

	_, err = cs.CoreV1().Services("default").Create(
		context.Background(), ctrlService("c", nil), metav1.CreateOptions{})
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
	pool, err := ParsePool("192.0.2.0/29", nil)
	require.NoError(t, err)
	cs, rec, _ := ctrlHarness(t, pool)

	seeded := ctrlService("stale", nil)
	seeded.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: "203.0.113.9"}}
	_, err = cs.CoreV1().Services("default").Create(context.Background(), seeded, metav1.CreateOptions{})
	require.NoError(t, err)

	ctrlAwaitIngress(t, cs, "stale", "192.0.2.1")
	ctrlAwaitEvent(t, rec, "Allocated")
}

// A class-scoped controller allocates for a Service carrying exactly its
// loadBalancerClass.
func TestControllerAllocatesMatchingClassService(t *testing.T) {
	pool, err := ParsePool("192.0.2.0/29", nil)
	require.NoError(t, err)
	cs, rec, _ := ctrlHarnessWithClass(t, pool, "nylon")

	_, err = cs.CoreV1().Services("default").Create(
		context.Background(), ctrlService("web", ctrlWithClass("nylon")), metav1.CreateOptions{})
	require.NoError(t, err)

	ctrlAwaitIngress(t, cs, "web", "192.0.2.1")
	ctrlAwaitEvent(t, rec, "Allocated")
}

// A Service carrying a foreign loadBalancerClass is never given an ingress
// and produces no events: it belongs to another controller.
func TestControllerIgnoresForeignClassService(t *testing.T) {
	pool, err := ParsePool("192.0.2.0/29", nil)
	require.NoError(t, err)
	cs, rec, _ := ctrlHarnessWithClass(t, pool, "nylon")

	_, err = cs.CoreV1().Services("default").Create(
		context.Background(), ctrlService("foreign", ctrlWithClass("other")), metav1.CreateOptions{})
	require.NoError(t, err)

	assert.Never(t, func() bool {
		return len(ctrlStatusIngress(t, cs, "foreign")) > 0
	}, time.Second, 50*time.Millisecond, "foreign-class Service must never get an ingress")
	assert.Empty(t, ctrlDrainEvents(rec), "foreign-class Service must produce no events")
}

// While --lb-class is set, an unclassed Service stays pending for another
// controller: strict match, not fallback-to-unclaimed.
func TestControllerIgnoresUnclassedServiceWhenClassSet(t *testing.T) {
	pool, err := ParsePool("192.0.2.0/29", nil)
	require.NoError(t, err)
	cs, _, _ := ctrlHarnessWithClass(t, pool, "nylon")

	_, err = cs.CoreV1().Services("default").Create(
		context.Background(), ctrlService("unclassed", nil), metav1.CreateOptions{})
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
	pool, err := ParsePool("192.0.2.0/29", nil)
	require.NoError(t, err)
	cs, rec, _ := ctrlHarnessWithClass(t, pool, "nylon")

	_, err = cs.CoreV1().Services("default").Create(
		context.Background(), ctrlService("flip", ctrlWithClass("nylon")), metav1.CreateOptions{})
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
	pool, err := ParsePool("192.0.2.0/29", nil)
	require.NoError(t, err)
	cs, _, _ := ctrlHarnessWithClass(t, pool, "nylon")

	seeded := ctrlService("foreign", ctrlWithClass("other"))
	seeded.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: "203.0.113.9"}}
	_, err = cs.CoreV1().Services("default").Create(context.Background(), seeded, metav1.CreateOptions{})
	require.NoError(t, err)

	assert.Never(t, func() bool {
		return !slices.Equal(ctrlStatusIngress(t, cs, "foreign"), []string{"203.0.113.9"})
	}, time.Second, 50*time.Millisecond, "foreign-class status must stay untouched")
}

// The taken set is class-agnostic: an in-pool address held by a
// foreign-class Service still counts as taken, so it is never handed to an
// owned Service — overlapping pools must never double-assign.
func TestControllerForeignInPoolIngressCountsAsTaken(t *testing.T) {
	pool, err := ParsePool("192.0.2.0/30", nil) // usable: .1, .2
	require.NoError(t, err)
	cs, _, _ := ctrlHarnessWithClass(t, pool, "nylon")

	foreign := ctrlService("foreign", ctrlWithClass("other"))
	foreign.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: "192.0.2.1"}}
	_, err = cs.CoreV1().Services("default").Create(context.Background(), foreign, metav1.CreateOptions{})
	require.NoError(t, err)

	_, err = cs.CoreV1().Services("default").Create(
		context.Background(), ctrlService("web", ctrlWithClass("nylon")), metav1.CreateOptions{})
	require.NoError(t, err)

	ctrlAwaitIngress(t, cs, "web", "192.0.2.2")
	assert.Equal(t, []string{"192.0.2.1"}, ctrlStatusIngress(t, cs, "foreign"),
		"the foreign-class holder keeps its address (hands off)")
}
