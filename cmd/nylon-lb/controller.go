package main

import (
	"context"
	"log/slog"
	"net/netip"
	"slices"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/wait"
	informers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
)

// Event reasons emitted by the allocator. These are pinned vocabulary: other
// tooling matches on them, so they must not be reworded.
const (
	reasonAllocated             = "Allocated"
	reasonReleased              = "Released"
	reasonAllocationFailed      = "AllocationFailed"
	reasonBadLoadBalancerIP     = "BadLoadBalancerIP"
	reasonMissingPoolAnnotation = "MissingPoolAnnotation"
	reasonUnknownPool           = "UnknownPool"
)

// PoolAnnotation selects which named pool allocates a Service's ingress IP.
const PoolAnnotation = "nylon.io/lb-pool"

// poolExhaustedError is returned when Allocate finds no free address. The
// worker treats any non-nil reconcile error as a rate-limited requeue, so an
// exhausted pool backs off exponentially instead of hot-looping.
type poolExhaustedError struct{ key string }

func (e poolExhaustedError) Error() string {
	return "no load balancer IP available for " + e.key + ": pool exhausted"
}

// Controller is the allocator half of nylon-lb: it gives every LoadBalancer
// Service it owns (empty --lb-class: all of them; otherwise exactly those
// with a matching spec.loadBalancerClass) one ingress IP from the pool its
// nylon.io/lb-pool annotation selects, and writes it to
// status.loadBalancer.ingress. Announcing the /32 into the mesh is the
// speaker's job. A Service whose annotation is missing or names an unknown
// pool gets no address, and any stale in-pool ingress this controller
// allocated for it is released. Allocation state is derived, never stored:
// the taken set is recomputed from the live Services on every reconcile, so
// the controller keeps no bookkeeping and restarts without losing anything.
type Controller struct {
	client   kubernetes.Interface
	factory  informers.SharedInformerFactory
	pools    *PoolSet
	lbClass  string               // empty claims all LoadBalancer Services (see ownsService)
	recorder record.EventRecorder // nil disables events
	logger   *slog.Logger
	metrics  *Metrics // nil disables metrics
	// allocMu serializes the derive→allocate→updateStatus critical section
	// across the worker pool: two concurrent reconciles could otherwise both
	// observe the same free address and hand it to two Services. The leader
	// is a single process, so one mutex closes the window entirely.
	queue   workqueue.TypedRateLimitingInterface[string]
	allocMu sync.Mutex

	// svcSynced closes over the service informer's HasSynced; Run blocks on it.
	svcSynced cache.InformerSynced

	// stateTrigger wakes RunStateMetrics after a Service event; capacity 1 so
	// a burst of events coalesces into at most one pending refresh.
	stateTrigger chan struct{}

	// ctx is captured at Run startup, before any worker goroutine exists, so
	// reconcile can bind API calls to the controller lifetime. It is never
	// written again afterwards.
	ctx context.Context
}

// ControllerOptions configures NewController.
type ControllerOptions struct {
	Client   kubernetes.Interface
	Factory  informers.SharedInformerFactory
	Pools    *PoolSet
	LBClass  string               // empty claims all LoadBalancer Services
	Recorder record.EventRecorder // nil = no events
	Logger   *slog.Logger
	Metrics  *Metrics // nil disables metrics
}

// NewController wires the allocator: a Service informer feeding a rate-limited
// workqueue with the default controller backoff. The factory is not started
// here; the owner starts it and then calls Run.
func NewController(opts ControllerOptions) *Controller {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	svcInformer := opts.Factory.Core().V1().Services()
	c := &Controller{
		client:       opts.Client,
		factory:      opts.Factory,
		pools:        opts.Pools,
		lbClass:      opts.LBClass,
		recorder:     opts.Recorder,
		logger:       logger,
		metrics:      opts.Metrics,
		queue:        workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]()),
		svcSynced:    svcInformer.Informer().HasSynced,
		stateTrigger: make(chan struct{}, 1),
	}
	_, err := svcInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: c.enqueue,
		UpdateFunc: func(_, newObj interface{}) {
			// Resyncs re-deliver unchanged objects; that is fine — the
			// reconcile keep-branch makes those no-ops. Filtering on
			// ResourceVersion movement would drop real updates from
			// API surfaces that do not bump it (the fake clientset).
			c.enqueue(newObj)
		},
		DeleteFunc: c.enqueueDeleted,
	})
	if err != nil {
		// Only possible on a factory that already delivered events; the
		// handler would simply never fire, so log and continue.
		logger.Error("registering service event handlers", "error", err)
	}
	return c
}

// enqueue converts an informer object into its namespace/name key and queues
// it for reconciliation.
func (c *Controller) enqueue(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		c.logger.Warn("skipping object without a usable key", "error", err)
		return
	}
	c.queue.Add(key)
	c.wakeStateMetrics()
}

// enqueueDeleted is the delete-side handler; DeletionHandlingMetaNamespaceKeyFunc
// survives tombstones, so a delete observed only as DeletedFinalStateUnknown
// still reconciles.
func (c *Controller) enqueueDeleted(obj interface{}) {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		c.logger.Warn("skipping deleted object without a usable key", "error", err)
		return
	}
	c.queue.Add(key)
	c.wakeStateMetrics()
}

// Run blocks until ctx is done: it waits for the informer cache to sync, then
// runs two reconcile workers. The caller owns factory.Start; Run never starts
// it. It returns nil once the context is done and the queue is shut down.
func (c *Controller) Run(ctx context.Context) error {
	c.ctx = ctx
	defer c.queue.ShutDown()

	c.logger.Info("starting load balancer allocator")
	if !cache.WaitForCacheSync(ctx.Done(), c.svcSynced) {
		c.logger.Error("service informer cache failed to sync")
		return ctx.Err()
	}

	for range 2 {
		go wait.Until(c.runWorker, time.Second, ctx.Done())
	}

	<-ctx.Done()
	c.logger.Info("stopping load balancer allocator")
	return nil
}

// RunStateMetrics blocks until ctx is done, maintaining the cluster-wide
// allocation gauges from the Service informer cache: nylon_lb_services and
// nylon_lb_allocated_ips describe the cluster, not this process's own
// actions, so EVERY replica runs this loop (never leader-elected) and reports
// the same values — a leader change or restart must not zero or freeze them.
// The caller owns factory.Start; this waits for the cache itself. Always
// returns nil on ctx cancellation.
func (c *Controller) RunStateMetrics(ctx context.Context) error {
	if c.metrics == nil {
		return nil
	}
	if !cache.WaitForCacheSync(ctx.Done(), c.svcSynced) {
		return ctx.Err()
	}
	c.refreshStateMetrics()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-c.stateTrigger:
			c.refreshStateMetrics()
		}
	}
}

// refreshStateMetrics recomputes both cluster-wide gauges from the informer
// cache. Every Service owned by this controller counts toward
// nylon_lb_services, allocated or not; every owned Service whose status holds
// exactly one IPv4 address inside a configured pool counts its address toward
// nylon_lb_allocated_ips (serviceIngressIP — the allocator's output, which
// does not consult the annotation that produced it). The difference between
// the two gauges is the number of owned Services still waiting for an
// address, which is what makes a fail-closed Service (missing or unknown pool
// annotation, exhausted pool, status write not yet observed) visible in the
// metrics instead of silent. A lister failure leaves the previous values in
// place and counts as a "list" error, matching the speaker's handling.
func (c *Controller) refreshStateMetrics() {
	services, err := c.factory.Core().V1().Services().Lister().List(labels.Everything())
	if err != nil {
		c.logger.Warn("listing services for state metrics", "error", err)
		c.metrics.addError("list")
		return
	}
	owned := 0
	allocated := make(map[netip.Addr]struct{})
	for _, svc := range services {
		if !ownsService(svc, c.lbClass) {
			continue
		}
		owned++
		if ip, ok := serviceIngressIP(svc, c.pools, c.lbClass); ok {
			allocated[ip] = struct{}{}
		}
	}
	c.metrics.Services.Store(int64(owned))
	c.metrics.AllocatedIPs.Store(int64(len(allocated)))
}

// wakeStateMetrics nudges the state-metrics loop from an informer event
// without ever blocking the informer's delivery goroutine. The loop re-reads
// the cache, so a burst collapsing into at most one pending refresh loses
// nothing.
func (c *Controller) wakeStateMetrics() {
	select {
	case c.stateTrigger <- struct{}{}:
	default:
	}
}

// runWorker drains the queue until it shuts down.
func (c *Controller) runWorker() {
	for c.processNextWorkItem() {
	}
}

// processNextWorkItem reconciles one key with the standard workqueue hygiene:
// rate-limited requeue on error, Forget on success.
func (c *Controller) processNextWorkItem() bool {
	key, shutdown := c.queue.Get()
	if shutdown {
		return false
	}
	defer c.queue.Done(key)

	if err := c.reconcile(key); err != nil {
		c.logger.Error("failed to reconcile service", "key", key, "error", err)
		if c.metrics != nil {
			c.metrics.addError("reconcile")
		}
		c.queue.AddRateLimited(key)
		return true
	}
	c.queue.Forget(key)
	return true
}

// reqCtx returns the context in-flight API calls run under: the Run lifetime
// once running, Background before that.
func (c *Controller) reqCtx() context.Context {
	if c.ctx != nil {
		return c.ctx
	}
	return context.Background()
}

// reconcile applies the allocation decision tree for one Service key:
//
//  1. Service gone -> nothing to do.
//  2. Not ours: a LoadBalancer Service outside our class (including unclassed
//     ones while --lb-class is set) -> hands off, status untouched; a
//     non-LoadBalancer Service -> release our leftover in-pool ingress
//     (Released), foreign entries untouched.
//  3. Ours but no pool selected: the nylon.io/lb-pool annotation is missing
//     or names an unknown pool -> fail closed: release our in-pool ingress
//     (Released) and warn (MissingPoolAnnotation / UnknownPool); the
//     Service stays pending.
//  4. Status already holds exactly one usable IPv4 inside the selected
//     pool -> keep it.
//  5. Otherwise re-derive the taken set from every live Service (global
//     across pools) and allocate: the requested spec.loadBalancerIP wins
//     when it is a free address inside the selected pool (BadLoadBalancerIP
//     warning otherwise), then the selected pool's lowest free address
//     (AllocationFailed warning when exhausted).
//
// Conflicts requeue rate-limited; the next pass re-fetches and wins the race.
func (c *Controller) reconcile(key string) error {
	ctx := c.reqCtx()
	ns, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		// An unparsable key can never become valid; drop it instead of
		// retrying forever.
		c.logger.Warn("dropping malformed service key", "key", key, "error", err)
		return nil
	}

	svc, err := c.factory.Core().V1().Services().Lister().Services(ns).Get(name)
	if apierrors.IsNotFound(err) {
		c.logger.Debug("service is gone; nothing to do", "key", key)
		return nil
	}
	if err != nil {
		return err
	}

	// A LoadBalancer Service this controller does not own — a different
	// or absent spec.loadBalancerClass while --lb-class scopes it —
	// belongs to another controller. Hands off its status entirely:
	// never allocate for it, and never clear an address this controller
	// did not allocate itself.
	if !ownsService(svc, c.lbClass) {
		if svc.Spec.Type == corev1.ServiceTypeLoadBalancer {
			return nil
		}
		// Not a LoadBalancer Service (anymore): release the in-pool
		// ingress so the address returns to the pool.
		return c.releaseIngress(ctx, key, ns, svc)
	}

	// Selection: the nylon.io/lb-pool annotation names the pool that
	// allocates this Service's address. Missing or unknown -> fail closed:
	// never allocate, and release any in-pool ingress this controller
	// previously allocated so the address returns to its pool. A plain
	// return (no error) relies on resyncs and annotation-update informer
	// events to re-trigger; the repeating warning matches the
	// AllocationFailed-on-exhaustion precedent and the recorder aggregates
	// same-reason events.
	poolName := svc.Annotations[PoolAnnotation]
	selected, known := c.pools.Get(poolName)
	if !known {
		if err := c.releaseIngress(ctx, key, ns, svc); err != nil {
			return err
		}
		if poolName == "" {
			c.event(svc, corev1.EventTypeWarning, reasonMissingPoolAnnotation,
				"no "+PoolAnnotation+" annotation: no pool selected; not allocating")
		} else {
			c.event(svc, corev1.EventTypeWarning, reasonUnknownPool,
				PoolAnnotation+" annotation references unknown pool \""+poolName+"\"; not allocating")
		}
		return nil
	}

	// Idempotent fast path: exactly one usable IPv4 inside the selected pool
	// is the state the allocator leaves Services in, so resyncs stop here
	// without churn. Re-pointing the annotation at another pool fails this
	// check and forces reassignment below.
	if ip, ok := keepIngressIP(svc.Status.LoadBalancer.Ingress, selected); ok {
		c.logger.Debug("keeping existing load balancer allocation", "key", key, "ip", ip.String())
		return nil
	}

	// The derive→allocate→update section must be serialized across workers
	// (see allocMu); conflicts requeue and re-derive from scratch.
	c.allocMu.Lock()
	err = c.assignIngress(ctx, key, ns, name, selected)
	c.allocMu.Unlock()
	return err
}

// releaseIngress clears this controller's own in-pool ingress entries from
// svc so the addresses return to their pools; foreign entries are not ours
// to clear and stay untouched. It is a no-op when nothing of ours is left.
// Status conflicts requeue rate-limited; the next pass re-fetches and wins
// the race.
func (c *Controller) releaseIngress(ctx context.Context, key, ns string, svc *corev1.Service) error {
	mine := inPoolIngress(svc.Status.LoadBalancer.Ingress, c.pools)
	if len(mine) == 0 {
		return nil
	}
	released := ingressIPStrings(mine)
	var kept []corev1.LoadBalancerIngress
	for _, ing := range svc.Status.LoadBalancer.Ingress {
		if !slices.ContainsFunc(mine, func(m corev1.LoadBalancerIngress) bool { return m.IP == ing.IP }) {
			kept = append(kept, ing)
		}
	}
	updated := svc.DeepCopy()
	updated.Status.LoadBalancer.Ingress = kept
	_, err := c.client.CoreV1().Services(ns).UpdateStatus(ctx, updated, metav1.UpdateOptions{})
	if err != nil {
		if apierrors.IsConflict(err) {
			c.queue.AddRateLimited(key)
			return nil
		}
		return err
	}
	c.logger.Info("released load balancer ingress", "key", key, "ips", released)
	c.event(svc, corev1.EventTypeNormal, reasonReleased, "released load balancer ingress "+joinIPs(released))
	if c.metrics != nil {
		c.metrics.Releases.Add(1)
	}
	return nil
}

// assignIngress is the allocation half of reconcile, called with allocMu held:
// it re-derives the target from a live cluster-wide List (never the informer
// cache, which can lag behind this controller's own status writes), computes
// the taken set across every configured pool, honors a free
// spec.loadBalancerIP inside the selected pool, and otherwise allocates the
// selected pool's lowest free address.
func (c *Controller) assignIngress(ctx context.Context, key, ns, name string, selected *Pool) error {
	list, err := c.client.CoreV1().Services("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	taken := make(map[netip.Addr]struct{})
	var svc *corev1.Service
	for i := range list.Items {
		s := &list.Items[i]
		if s.Namespace == ns && s.Name == name {
			svc = s
			// The target's own requested address is not "taken by
			// another service": honoring spec.loadBalancerIP for
			// this very Service is the point of the field. Its
			// status ingress still counts, so a stale multi-entry
			// status cannot get its own addresses handed back out.
			takeServiceIPs(taken, c.pools, s, false)
			continue
		}
		takeServiceIPs(taken, c.pools, s, true)
	}
	if svc == nil {
		c.logger.Debug("service is gone; nothing to do", "key", key)
		return nil
	}
	if !ownsService(svc, c.lbClass) {
		// The Service left our class between the informer view and this
		// live List: not ours to allocate. The reconcile gate hands it
		// off on the next pass.
		return nil
	}

	// A requested spec.loadBalancerIP wins when it is free and inside the
	// selected pool; any other request warns and falls through to a pool
	// allocation.
	candidate, requested := netip.Addr{}, svc.Spec.LoadBalancerIP
	if requested != "" {
		ip, perr := netip.ParseAddr(requested)
		switch {
		case perr != nil || !ip.Is4() || !selected.Contains(ip):
			c.event(svc, corev1.EventTypeWarning, reasonBadLoadBalancerIP,
				"requested LoadBalancerIP "+requested+" is outside the selected pool; allocating a pool address instead")
		default:
			if _, busy := taken[ip]; busy {
				c.event(svc, corev1.EventTypeWarning, reasonBadLoadBalancerIP,
					"requested LoadBalancerIP "+requested+" is already taken by another service; allocating a pool address instead")
			} else {
				candidate = ip
			}
		}
	}

	ip := candidate
	if !ip.IsValid() {
		var ok bool
		ip, ok = selected.Allocate(taken)
		if !ok {
			c.event(svc, corev1.EventTypeWarning, reasonAllocationFailed,
				"load balancer pool "+selected.name+" is exhausted; no address available")
			return poolExhaustedError{key: key}
		}
	}

	updated := svc.DeepCopy()
	updated.Status.LoadBalancer = corev1.LoadBalancerStatus{
		Ingress: []corev1.LoadBalancerIngress{{IP: ip.String()}},
	}
	_, err = c.client.CoreV1().Services(ns).UpdateStatus(ctx, updated, metav1.UpdateOptions{})
	if err != nil {
		if apierrors.IsConflict(err) {
			// Another writer touched the status between list and update;
			// requeue so the next pass re-fetches and converges.
			c.queue.AddRateLimited(key)
			return nil
		}
		return err
	}
	if c.metrics != nil {
		c.metrics.Allocations.Add(1)
	}
	if candidate.IsValid() {
		c.logger.Info("honored requested load balancer IP", "key", key, "ip", ip.String())
		c.event(svc, corev1.EventTypeNormal, reasonAllocated, "allocated requested load balancer IP "+ip.String())
	} else {
		c.logger.Info("allocated load balancer IP", "key", key, "ip", ip.String())
		c.event(svc, corev1.EventTypeNormal, reasonAllocated, "allocated load balancer IP "+ip.String())
	}
	return nil
}

// event emits an event on svc unless no recorder was configured.
func (c *Controller) event(svc *corev1.Service, eventtype, reason, message string) {
	if c.recorder == nil {
		return
	}
	c.recorder.Event(svc, eventtype, reason, message)
}

// ownsService reports whether svc belongs to this controller: a
// type=LoadBalancer Service that, when lbClass is set, carries exactly that
// spec.loadBalancerClass. An empty lbClass claims all LoadBalancer Services.
func ownsService(svc *corev1.Service, lbClass string) bool {
	if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
		return false
	}
	if lbClass == "" {
		return true
	}
	return svc.Spec.LoadBalancerClass != nil && *svc.Spec.LoadBalancerClass == lbClass
}

// keepIngressIP reports whether ingress is exactly one entry holding a
// non-empty IPv4 address inside the selected pool, returning that address.
// This is the settled state reconciliation aims for; selecting a different
// pool deliberately fails the check and forces reassignment.
func keepIngressIP(ingress []corev1.LoadBalancerIngress, pool *Pool) (netip.Addr, bool) {
	if len(ingress) != 1 || ingress[0].IP == "" {
		return netip.Addr{}, false
	}
	ip, err := netip.ParseAddr(ingress[0].IP)
	if err != nil || !ip.Is4() || !pool.Contains(ip) {
		return netip.Addr{}, false
	}
	return ip, true
}

// takeServiceIPs adds addresses claimed by s to taken: its status ingress
// IPs always, and its requested spec.loadBalancerIP only when withRequested
// is set (the reconciled Service's own request is a candidate, not a
// claim). Each address counts only when it is an IPv4 address inside any
// configured pool, which makes the taken set global across pools and keeps
// overlapping pools from double-assigning.
func takeServiceIPs(taken map[netip.Addr]struct{}, pools *PoolSet, s *corev1.Service, withRequested bool) {
	for _, ing := range s.Status.LoadBalancer.Ingress {
		if ip, err := netip.ParseAddr(ing.IP); err == nil && ip.Is4() && pools.Contains(ip) {
			taken[ip] = struct{}{}
		}
	}
	if withRequested {
		if req := s.Spec.LoadBalancerIP; req != "" {
			if ip, err := netip.ParseAddr(req); err == nil && ip.Is4() && pools.Contains(ip) {
				taken[ip] = struct{}{}
			}
		}
	}
}

// inPoolIngress returns the ingress entries holding an IPv4 address inside
// any configured pool — this controller's own allocations, whichever pool
// they came from.
func inPoolIngress(ingress []corev1.LoadBalancerIngress, pools *PoolSet) []corev1.LoadBalancerIngress {
	out := make([]corev1.LoadBalancerIngress, 0, len(ingress))
	for _, ing := range ingress {
		if ip, err := netip.ParseAddr(ing.IP); err == nil && ip.Is4() && pools.Contains(ip) {
			out = append(out, ing)
		}
	}
	return out
}

// ingressIPStrings returns the raw IP strings of a status ingress list.
func ingressIPStrings(ingress []corev1.LoadBalancerIngress) []string {
	ips := make([]string, 0, len(ingress))
	for _, ing := range ingress {
		ips = append(ips, ing.IP)
	}
	return ips
}

// joinIPs renders ips as a comma-separated list for event messages.
func joinIPs(ips []string) string {
	out := ""
	for i, ip := range ips {
		if i > 0 {
			out += ", "
		}
		out += ip
	}
	return out
}
