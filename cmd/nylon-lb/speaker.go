package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

// speakerDefaultDrainDelay is how long a withdrawn address stays bound
// after its announce file is removed: the mesh keeps routing flows here
// until it converges, and unbinding first would black-hole them.
const speakerDefaultDrainDelay = 60 * time.Second

// speakerAPICheckInterval is how often the speaker probes the API directly
// (an uncached read through the typed clientset); speakerAPIStaleTTL is how
// old that last success may get before a single-announcer Service is
// withdrawn — announcing from nobody beats announcing from two owners.
const (
	speakerAPICheckInterval = 10 * time.Second
	speakerAPIStaleTTL      = 30 * time.Second
)

// AnnounceAnnotation selects the announce mode for a LoadBalancer Service:
// "anycast" (default) announces from every node externalTrafficPolicy
// allows; "single" elects exactly one announcer among the nodes holding a
// ready local endpoint and requires externalTrafficPolicy: Local.
const AnnounceAnnotation = "nylon.io/announce"

const (
	announceModeAnycast = "anycast"
	announceModeSingle  = "single"
)

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
	// inside any configured pool — the set reconcile sweeps for drift.
	ListInPool() ([]netip.Addr, error)
}

// SpeakerOptions configures a Speaker.
type SpeakerOptions struct {
	Client      kubernetes.Interface
	Factory     informers.SharedInformerFactory
	Pools       *PoolSet
	LBClass     string // empty claims all LoadBalancer Services
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
// makes node failover automatic. A Service can opt out with the
// nylon.io/announce: single annotation, which elects one announcer among the
// nodes holding a ready local endpoint.
type Speaker struct {
	client        kubernetes.Interface
	pools         *PoolSet
	lbClass       string
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

	// drainDelay is how long an address stays bound after its announce file
	// is withdrawn; draining maps a withdrawn address to its unbind deadline
	// so the address survives until the mesh stops routing flows here.
	drainDelay time.Duration
	draining   map[netip.Addr]time.Time

	// Single-announcer election state: apiInterval is the direct API probe
	// cadence, apiStaleTTL how old the last success may get before
	// single-mode announces are withdrawn, lastAPIOK the unix-nano time of
	// that success (0 before the first probe), and singleElected the
	// Services this node currently announces under single mode (transition
	// logging).
	apiInterval   time.Duration
	apiStaleTTL   time.Duration
	lastAPIOK     atomic.Int64
	singleElected map[string]struct{}
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
	case opts.Pools == nil:
		return nil, errors.New("building speaker: address pools are required")
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
		pools:         opts.Pools,
		lbClass:       opts.LBClass,
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
		drainDelay:    speakerDefaultDrainDelay,
		draining:      make(map[netip.Addr]time.Time),
		apiInterval:   speakerAPICheckInterval,
		apiStaleTTL:   speakerAPIStaleTTL,
		singleElected: make(map[string]struct{}),
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
	// The cache sync just proved the API reachable; single-announcer
	// elections count on that view until the first direct probe renews it.
	s.lastAPIOK.Store(time.Now().UnixNano())
	go s.runAPIProbe(ctx)
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

// runAPIProbe refreshes lastAPIOK with a direct, uncached API read every
// apiInterval until ctx is done.
func (s *Speaker) runAPIProbe(ctx context.Context) {
	ticker := time.NewTicker(s.apiInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.probeAPI(ctx)
		}
	}
}

// probeAPI performs one direct API read (the typed clientset never reads the
// informer cache) and records the success time; failures are logged and
// leave the timestamp untouched.
func (s *Speaker) probeAPI(ctx context.Context) {
	reqCtx, cancel := context.WithTimeout(ctx, s.apiInterval)
	defer cancel()
	if _, err := s.client.DiscoveryV1().EndpointSlices(metav1.NamespaceAll).List(reqCtx, metav1.ListOptions{Limit: 1}); err != nil {
		s.log.Warn("api liveness probe failed", "error", err)
		return
	}
	s.lastAPIOK.Store(time.Now().UnixNano())
}

// apiFresh reports whether the last successful direct API probe is within
// apiStaleTTL; a speaker that never probed reports stale.
func (s *Speaker) apiFresh(now time.Time) bool {
	last := s.lastAPIOK.Load()
	return last != 0 && now.Sub(time.Unix(0, last)) <= s.apiStaleTTL
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
// deterministic reconciliation, plus the keys ("ns/name") of the Services it
// announces under single mode.
//
// A type=LoadBalancer Service owned by this controller (empty --lb-class:
// all of them) is announced when its status carries exactly one IPv4
// ingress address inside any configured pool (the allocator's output —
// the speaker never allocates) and:
//
//   - externalTrafficPolicy != Local (the Cluster default): always, under
//     any announce mode. Every node announces the /32; Babel anycast picks
//     the closest one and failover is automatic.
//   - externalTrafficPolicy == Local, mode anycast (the default): only
//     while at least one ready endpoint of the Service runs on this node.
//   - externalTrafficPolicy == Local, mode single (nylon.io/announce:
//     single): only on the one node singleAnnouncer elects among the nodes
//     holding a ready endpoint, and only while this node's direct API view
//     is fresh — a stateful path prefers a bounded black-hole over two
//     owners.
func (s *Speaker) desiredAnnounces() (out []announce, singleKeys []string) {
	now := time.Now()
	services, err := s.svcLister.List(labels.Everything())
	if err != nil {
		s.log.Warn("listing services", "error", err)
		if s.metrics != nil {
			s.metrics.addError("list")
		}
		return nil, nil
	}
	for _, svc := range services {
		ip, ok := serviceIngressIP(svc, s.pools, s.lbClass)
		if !ok {
			continue
		}
		local := svc.Spec.ExternalTrafficPolicy == corev1.ServiceExternalTrafficPolicyLocal
		mode := announceModeValue(svc)
		switch {
		case mode == announceModeSingle && !local:
			s.log.Warn("announce mode single requires externalTrafficPolicy: Local; announcing anycast",
				"service", svc.Namespace+"/"+svc.Name)
			mode = announceModeAnycast
		case mode != "" && mode != announceModeSingle && mode != announceModeAnycast:
			s.log.Warn("unknown announce mode; announcing anycast",
				"service", svc.Namespace+"/"+svc.Name, "mode", mode)
			mode = announceModeAnycast
		}
		if local {
			key := svc.Namespace + "/" + svc.Name
			eligible := s.eligibleNodes(svc.Namespace, svc.Name)
			if mode == announceModeSingle {
				if !s.apiFresh(now) {
					continue // stale own view: withdraw single-mode announces
				}
				if singleAnnouncer(key, eligible) != s.nodeName {
					continue
				}
				singleKeys = append(singleKeys, key)
			} else if !containsNode(eligible, s.nodeName) {
				continue
			}
		}
		out = append(out, announce{
			ns:   svc.Namespace,
			name: svc.Name,
			file: fileName(svc.Namespace, svc.Name),
			ip:   ip,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].file < out[j].file })
	sort.Strings(singleKeys)
	return out, singleKeys
}

// serviceIngressIP extracts the single IPv4 address from a Service's
// status.loadBalancer.ingress when it falls inside any configured pool, or
// reports that the Service holds no allocated pool address: not a LoadBalancer Service, not
// owned by this controller (a loadBalancerClass mismatch while --lb-class
// scopes it — a foreign-class Service holding an in-pool address is another
// controller's to announce), zero or ambiguous ingress entries, a non-IPv4
// address, or an address outside every configured pool (foreign or stale —
// replaced by the allocator, never announced by the speaker).
func serviceIngressIP(svc *corev1.Service, pools *PoolSet, lbClass string) (netip.Addr, bool) {
	if !ownsService(svc, lbClass) {
		return netip.Addr{}, false
	}
	ingress := svc.Status.LoadBalancer.Ingress
	if len(ingress) != 1 {
		return netip.Addr{}, false
	}
	ip, err := netip.ParseAddr(ingress[0].IP)
	if err != nil || !ip.Is4() || !pools.Contains(ip) {
		return netip.Addr{}, false
	}
	return ip, true
}

// announceModeValue returns the raw value of the announce annotation, or ""
// when the Service does not carry it.
func announceModeValue(svc *corev1.Service) string {
	return svc.Annotations[AnnounceAnnotation]
}

// eligibleNodes returns the sorted names of the nodes that hold at least one
// ready endpoint of Service ns/name. EndpointSlices are matched by namespace
// and the kubernetes.io/service-name label; an endpoint counts when NodeName
// is set and Conditions.Ready is nil or true (k8s semantics: nil means
// ready) — the same rule externalTrafficPolicy: Local applies. A lister
// error yields nil: no node is eligible, which is the fail-closed reading.
func (s *Speaker) eligibleNodes(ns, name string) []string {
	slices, err := s.sliceLister.EndpointSlices(ns).List(
		labels.SelectorFromSet(labels.Set{discoveryv1.LabelServiceName: name}))
	if err != nil {
		s.log.Warn("listing endpoint slices", "error", err, "service", ns+"/"+name)
		if s.metrics != nil {
			s.metrics.addError("list")
		}
		return nil
	}
	set := make(map[string]struct{})
	for _, slice := range slices {
		if slice.Namespace != ns {
			continue
		}
		for i := range slice.Endpoints {
			ep := &slice.Endpoints[i]
			if ep.NodeName == nil {
				continue
			}
			if ep.Conditions.Ready != nil && !*ep.Conditions.Ready {
				continue
			}
			set[*ep.NodeName] = struct{}{}
		}
	}
	nodes := make([]string, 0, len(set))
	for node := range set {
		nodes = append(nodes, node)
	}
	sort.Strings(nodes)
	return nodes
}

// singleAnnouncer returns the node elected to announce a single-mode Service
// key ("ns/name"), or "" when no node is eligible. It is a pure function of
// the key and the sorted eligible list, so every speaker computes the same
// winner without coordination.
func singleAnnouncer(key string, eligible []string) string {
	if len(eligible) == 0 {
		return ""
	}
	h := fnv.New64a()
	h.Write([]byte(key))
	return eligible[h.Sum64()%uint64(len(eligible))]
}

// containsNode reports whether the sorted node list contains node.
func containsNode(nodes []string, node string) bool {
	i := sort.SearchStrings(nodes, node)
	return i < len(nodes) && nodes[i] == node
}

// reconcile converges the node onto the desired state. The order inside is
// load-bearing: bind BEFORE announce, so the address already exists on the
// bind interface when the /32 propagates through the mesh (~250 ms after
// the file lands, per the daemon's watcher debounce); and withdraw the
// announce file BEFORE unbinding, so traffic stops being routed here before
// the address disappears. Withdrawal also starts a drain: the address stays
// bound for drainDelay after the file is gone, so flows the mesh still
// routes here survive until it converges on the withdrawal.
func (s *Speaker) reconcile() {
	s.mu.Lock()
	defer s.mu.Unlock()

	anns, singleKeys := s.desiredAnnounces()
	if s.metrics != nil {
		s.metrics.Announces.Store(int64(len(anns)))
	}
	s.logSingleTransitions(singleKeys)
	now := time.Now()

	// 1. Bind, then announce.
	for _, a := range anns {
		delete(s.draining, a.ip) // a re-desired address cancels a pending drain
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
				if !s.beginDrain(ip, now) {
					s.log.Info("draining withdrawn LB address", "ip", ip, "file", st.file, "delay", s.drainDelay)
					continue
				}
				if err := s.binder.Remove(ip); err != nil {
					s.log.Warn("unbinding withdrawn LB address", "error", err, "ip", ip, "file", st.file)
				} else {
					delete(s.bound, ip)
					delete(s.draining, ip)
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
				if deadline, ok := s.draining[ip]; ok && now.Before(deadline) {
					continue // still inside the drain window
				}
				if err := s.binder.Remove(ip); err != nil {
					s.log.Warn("removing drifted LB address", "error", err, "ip", ip)
				} else {
					delete(s.bound, ip)
					delete(s.draining, ip)
				}
			}
		}
	}
}

// logSingleTransitions records election changes for single-announcer
// Services: one line when this node becomes the elected announcer, one when
// it stops being it. reconcile calls this under s.mu.
func (s *Speaker) logSingleTransitions(keys []string) {
	next := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		next[key] = struct{}{}
		if _, ok := s.singleElected[key]; !ok {
			s.log.Info("announcing as elected single announcer", "service", key, "node", s.nodeName)
		}
	}
	for key := range s.singleElected {
		if _, ok := next[key]; !ok {
			s.log.Info("stopped announcing single-announcer service", "service", key)
		}
	}
	s.singleElected = next
}

// staleAnnounce is a withdrawable announce file together with the in-pool
// addresses it announced, parsed before the file is unlinked.
type staleAnnounce struct {
	file string
	ips  []netip.Addr
}

// beginDrain starts the drain window for an address this node withdraws and
// reports whether the unbind may proceed now. The first sighting starts the
// window; a zero drainDelay leaves it already over, so the address is
// unbound in the same pass (the pre-drain behaviour).
func (s *Speaker) beginDrain(ip netip.Addr, now time.Time) bool {
	deadline, ok := s.draining[ip]
	if !ok {
		deadline = now.Add(s.drainDelay)
		s.draining[ip] = deadline
	}
	return !now.Before(deadline)
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
			st.ips = fileAnnouncedIPs(data, s.pools)
		}
		out = append(out, st)
	}
	return out
}

// fileAnnouncedIPs parses announce-file content and returns the IPv4 /32
// addresses it holds inside any configured pool. Malformed content yields no
// addresses — the file is garbage-collected regardless, the unbind is merely
// skipped.
func fileAnnouncedIPs(data []byte, pools *PoolSet) []netip.Addr {
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
		if ip := prefix.Addr(); ip.Is4() && pools.Contains(ip) {
			ips = append(ips, ip)
		}
	}
	return ips
}
