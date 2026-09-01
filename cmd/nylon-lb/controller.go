package main

import (
	"context"
	"log/slog"
	"net/netip"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	reasonAllocated         = "Allocated"
	reasonReleased          = "Released"
	reasonAllocationFailed  = "AllocationFailed"
	reasonBadLoadBalancerIP = "BadLoadBalancerIP"
)

// poolExhaustedError is returned when Allocate finds no free address. The
// worker treats any non-nil reconcile error as a rate-limited requeue, so an
// exhausted pool backs off exponentially instead of hot-looping.
type poolExhaustedError struct{ key string }

func (e poolExhaustedError) Error() string {
	return "no load balancer IP available for " + e.key + ": pool exhausted"
}

// Controller is the allocator half of nylon-lb: it gives every LoadBalancer
// Service exactly one ingress IP from the pool and writes it to
// status.loadBalancer.ingress. Announcing the /32 into the mesh is the
// speaker's job. Allocation state is derived, never stored: the taken set is
// recomputed from the live Services on every reconcile, so the controller
// keeps no bookkeeping and restarts without losing anything.
type Controller struct {
	client   kubernetes.Interface
	factory  informers.SharedInformerFactory
	pool     *Pool
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

	// ctx is captured at Run startup, before any worker goroutine exists, so
	// reconcile can bind API calls to the controller lifetime. It is never
	// written again afterwards.
	ctx context.Context
}

// ControllerOptions configures NewController.
type ControllerOptions struct {
	Client   kubernetes.Interface
	Factory  informers.SharedInformerFactory
	Pool     *Pool
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
		client:    opts.Client,
		factory:   opts.Factory,
		pool:      opts.Pool,
		recorder:  opts.Recorder,
		logger:    logger,
		metrics:   opts.Metrics,
		queue:     workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]()),
		svcSynced: svcInformer.Informer().HasSynced,
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
//  2. Not a LoadBalancer Service -> clear any leftover ingress (Released).
//  3. Status already holds exactly one usable in-pool IPv4 -> keep it.
//  4. Otherwise re-derive the taken set from every live Service and allocate:
//     the requested spec.loadBalancerIP wins when it is a free in-pool
//     address (BadLoadBalancerIP warning otherwise), then the pool's lowest
//     free address (AllocationFailed warning when exhausted).
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

	// Anything that is not a LoadBalancer Service must not keep an address
	// allocated: clear a leftover status so the address returns to the pool.
	if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
		if len(svc.Status.LoadBalancer.Ingress) == 0 {
			return nil
		}
		released := ingressIPStrings(svc.Status.LoadBalancer.Ingress)
		updated := svc.DeepCopy()
		updated.Status.LoadBalancer = corev1.LoadBalancerStatus{}
		_, err = c.client.CoreV1().Services(ns).UpdateStatus(ctx, updated, metav1.UpdateOptions{})
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

	// Idempotent fast path: exactly one usable in-pool IPv4 is the state the
	// allocator leaves Services in, so resyncs stop here without churn.
	if ip, ok := keepIngressIP(svc.Status.LoadBalancer.Ingress, c.pool); ok {
		c.logger.Debug("keeping existing load balancer allocation", "key", key, "ip", ip.String())
		return nil
	}

	// The derive→allocate→update section must be serialized across workers
	// (see allocMu); conflicts requeue and re-derive from scratch.
	c.allocMu.Lock()
	err = c.assignIngress(ctx, key, ns, name)
	c.allocMu.Unlock()
	return err
}

// assignIngress is the allocation half of reconcile, called with allocMu held:
// it re-derives the target from a live cluster-wide List (never the informer
// cache, which can lag behind this controller's own status writes), computes
// the taken set, honors a free in-pool spec.loadBalancerIP, and otherwise
// allocates the pool's lowest free address.
func (c *Controller) assignIngress(ctx context.Context, key, ns, name string) error {
	list, err := c.client.CoreV1().Services("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	taken := make(map[netip.Addr]struct{})
	lbServices := 0
	var svc *corev1.Service
	for i := range list.Items {
		s := &list.Items[i]
		if s.Spec.Type == corev1.ServiceTypeLoadBalancer {
			lbServices++
		}
		if s.Namespace == ns && s.Name == name {
			svc = s
			// The target's own requested address is not "taken by
			// another service": honoring spec.loadBalancerIP for
			// this very Service is the point of the field. Its
			// status ingress still counts, so a stale multi-entry
			// status cannot get its own addresses handed back out.
			takeServiceIPs(taken, c.pool, s, false)
			continue
		}
		takeServiceIPs(taken, c.pool, s, true)
	}
	if svc == nil {
		c.logger.Debug("service is gone; nothing to do", "key", key)
		return nil
	}

	// A requested spec.loadBalancerIP wins when it is free and in-pool; any
	// other request warns and falls through to a pool allocation.
	candidate, requested := netip.Addr{}, svc.Spec.LoadBalancerIP
	if requested != "" {
		ip, perr := netip.ParseAddr(requested)
		switch {
		case perr != nil || !ip.Is4() || !c.pool.Contains(ip):
			c.event(svc, corev1.EventTypeWarning, reasonBadLoadBalancerIP,
				"requested LoadBalancerIP "+requested+" is outside the pool; allocating a pool address instead")
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
		ip, ok = c.pool.Allocate(taken)
		if !ok {
			c.event(svc, corev1.EventTypeWarning, reasonAllocationFailed,
				"load balancer pool is exhausted; no address available")
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
		c.metrics.AllocatedIPs.Store(int64(len(taken)))
		c.metrics.Services.Store(int64(lbServices))
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

// keepIngressIP reports whether ingress is exactly one entry holding a
// non-empty IPv4 address inside the pool, returning that address. This is the
// settled state reconciliation aims for.
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
// is set (the reconciled Service's own request is a candidate, not a claim).
// Each address counts only when it is an IPv4 address inside the pool.
func takeServiceIPs(taken map[netip.Addr]struct{}, pool *Pool, s *corev1.Service, withRequested bool) {
	for _, ing := range s.Status.LoadBalancer.Ingress {
		if ip, err := netip.ParseAddr(ing.IP); err == nil && ip.Is4() && pool.Contains(ip) {
			taken[ip] = struct{}{}
		}
	}
	if withRequested {
		if req := s.Spec.LoadBalancerIP; req != "" {
			if ip, err := netip.ParseAddr(req); err == nil && ip.Is4() && pool.Contains(ip) {
				taken[ip] = struct{}{}
			}
		}
	}
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
