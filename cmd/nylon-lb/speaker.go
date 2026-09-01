package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corev1lister "k8s.io/client-go/listers/core/v1"
	discoveryv1lister "k8s.io/client-go/listers/discovery/v1"
	"k8s.io/client-go/tools/cache"
)

// speakerDefaultResync is the safety-net reconcile interval used when
// SpeakerOptions.Resync is not set. Informer events and resyncs trigger on
// their own; this only bounds how long state that no event noticed (an
// externally written announce file, a manually added lo address) can drift.
const speakerDefaultResync = 30 * time.Second

// AddrBinder keeps announced addresses present on a local interface so the
// node actually answers traffic for the /32s it announces into the mesh.
// The speaker talks to this interface only; the netlink-backed production
// implementation lives in binder_linux.go (with a !linux stub), tests use an
// in-memory fake.
type AddrBinder interface {
	// Ensure idempotently binds ip (as a /32) on the interface.
	Ensure(ip netip.Addr) error
	// Remove unbinds ip; an absent address counts as success.
	Remove(ip netip.Addr) error
	// ListInPool returns the IPv4 addresses currently bound that fall
	// inside the pool — the set reconcile sweeps for drift.
	ListInPool() ([]netip.Addr, error)
}

// SpeakerOptions configures a Speaker.
type SpeakerOptions struct {
	Client      kubernetes.Interface
	Factory     informers.SharedInformerFactory
	Pool        *Pool
	PrefixesDir string
	NodeName    string
	Binder      AddrBinder // nil disables binding
	Logger      *slog.Logger
	Metrics     *Metrics      // nil disables metrics
	Resync      time.Duration // safety ticker; informer resync also triggers
}

// Speaker is the per-node announcer. Unlike the allocator it runs on every
// replica — the mesh announce is deliberately anycast: under the default
// externalTrafficPolicy=Cluster every node announces the Service's /32, and
// Babel routes traffic to whichever announce is closest, which is also what
// makes node failover automatic.
type Speaker struct {
	client        kubernetes.Interface
	pool          *Pool
	dir           string
	nodeName      string
	binder        AddrBinder
	log           *slog.Logger
	metrics       *Metrics // nil disables metrics
	resync        time.Duration
	svcLister     corev1lister.ServiceLister
	sliceLister   discoveryv1lister.EndpointSliceLister
	svcInformer   cache.SharedIndexInformer
	sliceInformer cache.SharedIndexInformer

	// trigger is the coalescing event channel: every informer handler tries
	// a non-blocking send, so a burst of events collapses into at most one
	// pending reconcile.
	trigger chan struct{}

	// mu guards the whole reconcile pass plus the bound set below it; the
	// bound set is the session's successfully ensured addresses and exists
	// to keep Ensure off the hot path on every resync (netlink churn).
	mu    sync.Mutex
	bound map[netip.Addr]struct{}
}

// NewSpeaker validates options and builds the speaker, registering handlers
// on the Service and EndpointSlice informers. The factory is not started
// here; Run owns the rest of the lifecycle.
func NewSpeaker(opts SpeakerOptions) (*Speaker, error) {
	switch {
	case opts.Client == nil:
		return nil, errors.New("building speaker: kubernetes client is required")
	case opts.Factory == nil:
		return nil, errors.New("building speaker: shared informer factory is required")
	case opts.Pool == nil:
		return nil, errors.New("building speaker: address pool is required")
	case opts.PrefixesDir == "":
		return nil, errors.New("building speaker: prefixes directory is required")
	case opts.NodeName == "":
		return nil, errors.New("building speaker: node name is required")
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	resync := opts.Resync
	if resync <= 0 {
		resync = speakerDefaultResync
	}

	svcInformer := opts.Factory.Core().V1().Services().Informer()
	sliceInformer := opts.Factory.Discovery().V1().EndpointSlices().Informer()
	s := &Speaker{
		client:        opts.Client,
		pool:          opts.Pool,
		dir:           opts.PrefixesDir,
		nodeName:      opts.NodeName,
		binder:        opts.Binder,
		log:           log,
		metrics:       opts.Metrics,
		resync:        resync,
		svcLister:     opts.Factory.Core().V1().Services().Lister(),
		sliceLister:   opts.Factory.Discovery().V1().EndpointSlices().Lister(),
		svcInformer:   svcInformer,
		sliceInformer: sliceInformer,
		trigger:       make(chan struct{}, 1),
		bound:         make(map[netip.Addr]struct{}),
	}
	handler := cache.ResourceEventHandlerFuncs{
		AddFunc:    func(interface{}) { s.kick() },
		UpdateFunc: func(interface{}, interface{}) { s.kick() },
		DeleteFunc: func(interface{}) { s.kick() },
	}
	// Registration on a factory-built informer cannot fail.
	_, _ = svcInformer.AddEventHandler(handler)
	_, _ = sliceInformer.AddEventHandler(handler)
	return s, nil
}

// kick coalesces an informer event into the trigger channel without ever
// blocking the informer's delivery goroutine. The event carries only the
// fact that something changed — reconcile reads the listers' current state
// — so dropping redundant notifications is safe.
func (s *Speaker) kick() {
	select {
	case s.trigger <- struct{}{}:
	default:
	}
}

// Run blocks until ctx is done, reconciling once after the caches sync, on
// every (coalesced) informer event, and on the resync ticker. It does not
// start the factory; the caller owns factory lifecycle. Always returns nil
// on ctx cancellation.
func (s *Speaker) Run(ctx context.Context) error {
	if !cache.WaitForCacheSync(ctx.Done(), s.svcInformer.HasSynced, s.sliceInformer.HasSynced) {
		return fmt.Errorf("speaker: informer caches did not sync: %w", context.Cause(ctx))
	}
	s.reconcile()

	ticker := time.NewTicker(s.resync)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			s.reconcile()
		case <-s.trigger:
			s.reconcile()
		}
	}
}

// announce is one desired mesh announce: Service ns/name holds ingress IP ip,
// published under the prefix file named file.
type announce struct {
	ns   string
	name string
	file string
	ip   netip.Addr
}

// desiredAnnounces is the pure function of the informer listers producing
// the announces this node should currently hold, sorted by file name for
// deterministic reconciliation.
//
// A type=LoadBalancer Service is announced when its status carries exactly
// one IPv4 ingress address inside the pool (the allocator's output — the
// speaker never allocates) and:
//
//   - externalTrafficPolicy != Local (the Cluster default): always. Every
//     node announces the /32; Babel anycast picks the closest one and
//     failover is automatic.
//   - externalTrafficPolicy == Local: only while at least one ready endpoint
//     runs on this node.
func (s *Speaker) desiredAnnounces() []announce {
	services, err := s.svcLister.List(labels.Everything())
	if err != nil {
		s.log.Warn("listing services", "error", err)
		if s.metrics != nil {
			s.metrics.addError("list")
		}
		return nil
	}
	var out []announce
	for _, svc := range services {
		ip, ok := serviceIngressIP(svc, s.pool)
		if !ok {
			continue
		}
		if svc.Spec.ExternalTrafficPolicy == corev1.ServiceExternalTrafficPolicyLocal &&
			!s.hasLocalEndpoint(svc.Namespace, svc.Name) {
			continue
		}
		out = append(out, announce{
			ns:   svc.Namespace,
			name: svc.Name,
			file: fileName(svc.Namespace, svc.Name),
			ip:   ip,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].file < out[j].file })
	return out
}

// serviceIngressIP extracts the single pool-contained IPv4 address from a
// Service's status.loadBalancer.ingress, or reports that there is nothing to
// announce: not a LoadBalancer Service, zero or ambiguous ingress entries,
// a non-IPv4 address, or an address outside the pool (foreign or stale —
// replaced by the allocator, never announced by the speaker).
func serviceIngressIP(svc *corev1.Service, pool *Pool) (netip.Addr, bool) {
	if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
		return netip.Addr{}, false
	}
	ingress := svc.Status.LoadBalancer.Ingress
	if len(ingress) != 1 {
		return netip.Addr{}, false
	}
	ip, err := netip.ParseAddr(ingress[0].IP)
	if err != nil || !ip.Is4() || !pool.Contains(ip) {
		return netip.Addr{}, false
	}
	return ip, true
}

// hasLocalEndpoint reports whether any EndpointSlice of Service ns/name (the
// kubernetes.io/service-name label links slices to their service) places a
// ready endpoint on this node. Readiness follows k8s semantics:
// Conditions.Ready == nil counts as ready. The union across a service's
// slices is what Local policy requires — any slice placing a ready endpoint
// here makes the service locally reachable.
func (s *Speaker) hasLocalEndpoint(ns, name string) bool {
	slices, err := s.sliceLister.EndpointSlices(ns).List(
		labels.SelectorFromSet(labels.Set{discoveryv1.LabelServiceName: name}))
	if err != nil {
		s.log.Warn("listing endpoint slices", "error", err, "service", ns+"/"+name)
		if s.metrics != nil {
			s.metrics.addError("list")
		}
		return false
	}
	for _, slice := range slices {
		for i := range slice.Endpoints {
			ep := &slice.Endpoints[i]
			if ep.NodeName == nil || *ep.NodeName != s.nodeName {
				continue
			}
			if ep.Conditions.Ready == nil || *ep.Conditions.Ready {
				return true
			}
		}
	}
	return false
}

// reconcile converges the node onto the desired state. The order inside is
// load-bearing: bind BEFORE announce, so the address already exists on the
// bind interface when the /32 propagates through the mesh (~250 ms after
// the file lands, per the daemon's watcher debounce); and withdraw the
// announce file BEFORE unbinding, so traffic stops being routed here before
// the address disappears.
func (s *Speaker) reconcile() {
	s.mu.Lock()
	defer s.mu.Unlock()

	anns := s.desiredAnnounces()
	if s.metrics != nil {
		s.metrics.Announces.Store(int64(len(anns)))
	}

	// 1. Bind, then announce.
	for _, a := range anns {
		if s.binder != nil {
			if _, ensured := s.bound[a.ip]; !ensured {
				if err := s.binder.Ensure(a.ip); err != nil {
					// Announcing without the local bind is better than not
					// announcing: traffic still reaches the service via
					// other announcing nodes, and the next reconcile
					// retries the bind.
					s.log.Warn("binding LB address", "error", err, "ip", a.ip, "service", a.ns+"/"+a.name)
					if s.metrics != nil {
						s.metrics.addError("bind")
					}
				} else {
					s.bound[a.ip] = struct{}{}
				}
			}
		}
		if _, err := writePrefixFile(s.dir, a.ns, a.name, a.ip); err != nil {
			s.log.Warn("writing announce file", "error", err, "file", a.file)
			if s.metrics != nil {
				s.metrics.addError("write")
			}
		} else if s.metrics != nil {
			s.metrics.AnnounceWrites.Add(1)
		}
	}

	// 2. Withdraw announces we no longer hold. The stale files are read
	// before gcPrefixFiles unlinks them — their content is what tells us
	// which addresses to unbind.
	desiredFiles := make(map[string]struct{}, len(anns))
	desiredIPs := make(map[netip.Addr]struct{}, len(anns))
	for _, a := range anns {
		desiredFiles[a.file] = struct{}{}
		desiredIPs[a.ip] = struct{}{}
	}
	stale := s.staleAnnounces(desiredFiles)
	if _, err := gcPrefixFiles(s.dir, desiredFiles); err != nil {
		s.log.Warn("garbage-collecting announce files", "error", err, "dir", s.dir)
	}
	if s.binder != nil {
		for _, st := range stale {
			for _, ip := range st.ips {
				if err := s.binder.Remove(ip); err != nil {
					s.log.Warn("unbinding withdrawn LB address", "error", err, "ip", ip, "file", st.file)
				} else {
					delete(s.bound, ip)
				}
			}
		}
	}

	// 3. Fallback sweep: any address still live inside the pool but not
	// desired was left behind by a crash between bind and withdraw, or by a
	// missed event — take it down.
	if s.binder != nil {
		live, err := s.binder.ListInPool()
		if err != nil {
			s.log.Warn("listing bound LB addresses", "error", err)
		} else {
			for _, ip := range live {
				if _, ok := desiredIPs[ip]; ok {
					continue
				}
				if err := s.binder.Remove(ip); err != nil {
					s.log.Warn("removing drifted LB address", "error", err, "ip", ip)
				} else {
					delete(s.bound, ip)
				}
			}
		}
	}
}

// staleAnnounce is a withdrawable announce file together with the in-pool
// addresses it announced, parsed before the file is unlinked.
type staleAnnounce struct {
	file string
	ips  []netip.Addr
}

// staleAnnounces globs the controller's lb-*.json files and reads each one
// not in keep, extracting the in-pool /32 addresses it announced. Unreadable
// or unparsable files are still withdrawn by the caller's GC; they simply
// have nothing to unbind.
func (s *Speaker) staleAnnounces(keep map[string]struct{}) []staleAnnounce {
	matches, err := filepath.Glob(filepath.Join(s.dir, "lb-*.json"))
	if err != nil {
		s.log.Warn("globbing announce files", "error", err, "dir", s.dir)
		if s.metrics != nil {
			s.metrics.addError("glob")
		}
		return nil
	}
	var out []staleAnnounce
	for _, path := range matches {
		file := filepath.Base(path)
		if _, ok := keep[file]; ok {
			continue
		}
		st := staleAnnounce{file: file}
		if data, err := os.ReadFile(path); err == nil {
			st.ips = fileAnnouncedIPs(data, s.pool)
		}
		out = append(out, st)
	}
	return out
}

// fileAnnouncedIPs parses announce-file content and returns the in-pool IPv4
// /32 addresses it holds. Malformed content yields no addresses — the file
// is garbage-collected regardless, the unbind is merely skipped.
func fileAnnouncedIPs(data []byte, pool *Pool) []netip.Addr {
	var file lbPrefixFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil
	}
	var ips []netip.Addr
	for _, entry := range file.Prefixes {
		prefix, err := netip.ParsePrefix(entry.Prefix)
		if err != nil || !prefix.IsSingleIP() {
			continue
		}
		if ip := prefix.Addr(); ip.Is4() && pool.Contains(ip) {
			ips = append(ips, ip)
		}
	}
	return ips
}
