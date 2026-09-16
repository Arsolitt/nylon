// nylon-lb is the Kubernetes LoadBalancer controller for the nylon mesh.
//
// It allocates an IP from a configured pool for every type=LoadBalancer
// Service, publishes it via status.loadBalancer.ingress (Cilium's
// kube-proxy-replacement serves the data plane from there), and announces the
// /32 into the mesh by writing a dynamic-prefix file into the node's
// prefixes.d dir and binding the address on the bind interface — the same
// reference VIP pattern as nylon-vip.service plus 00-vip.json.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/encodeous/nylon/internal/buildinfo"
	"github.com/encodeous/nylon/internal/logging"
	"github.com/encodeous/nylon/log"
	"github.com/go-logr/logr"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
)

const (
	leaseName         = "nylon-lb-allocator"
	saNamespacePath   = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
	fallbackNamespace = "kube-system"
)

// lbOptions is the parsed flag set; bound by newRootCmd, consumed by run.
type lbOptions struct {
	pools           []string
	excludes        []string
	lbClass         string
	prefixesDir     string
	bindInterface   string
	kubeconfig      string
	nodeName        string
	leaderElect     bool
	leaderNamespace string
	resync          time.Duration
	healthAddr      string
	speaker         bool
	allocator       bool
	logLevel        string
	verbose         bool
	json            bool
}

func main() {
	if err := newRootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	var opts lbOptions

	cmd := &cobra.Command{
		Use:   "nylon-lb",
		Short: "Nylon mesh LoadBalancer controller for Kubernetes Services",
		Long: `nylon-lb watches type=LoadBalancer Services and, for each one, allocates
an IP from the pool its nylon.io/lb-pool annotation selects, publishes it via
status.loadBalancer.ingress, and announces the /32 into the nylon mesh by
writing a dynamic-prefix file into the node's prefixes.d dir and binding the
address on the bind interface. Cilium's kube-proxy-replacement consumes
status.loadBalancer.ingress and serves the data plane; this controller owns
only allocation and announce.

Pools are named and repeatable (--pool name=cidr); every LoadBalancer
Service must carry the nylon.io/lb-pool annotation naming one of them.
A Service with a missing or unknown pool annotation gets no address and
any stale allocation for it is released.

The allocator (status writes) runs leader-elected cluster-wide; the speaker
(announce + bind) runs on every node so per-node announce placement
(externalTrafficPolicy=Cluster announces everywhere, Local only where ready
endpoints live) is decided locally.

When --lb-class names a loadBalancerClass, only Services carrying exactly
that spec.loadBalancerClass are reconciled; Services with another or no
class are left to their own controllers. An empty --lb-class claims every
LoadBalancer Service.`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return run(&opts)
		},
	}

	cmd.Version = buildinfo.Version

	cmd.Flags().StringArrayVar(&opts.pools, "pool", nil, "named IPv4 pool to allocate LoadBalancer ingress IPs from (repeatable; name=cidr, e.g. shared=10.110.0.0/24)")
	cmd.Flags().StringArrayVar(&opts.excludes, "exclude", nil, "pool IP to never allocate (repeatable; e.g. a probe address that must stay free)")
	cmd.Flags().StringVar(&opts.lbClass, "lb-class", "", "loadBalancerClass to claim; empty claims all type=LoadBalancer Services")
	cmd.Flags().StringVar(&opts.prefixesDir, "prefixes-dir", "/etc/nylon/prefixes.d", "nylon dynamic_prefixes_dir to write announce files into")
	cmd.Flags().StringVar(&opts.bindInterface, "bind-interface", "lo", "interface to bind allocated IPs on (empty string disables binding)")
	cmd.Flags().StringVar(&opts.kubeconfig, "kubeconfig", "", "path to a kubeconfig (empty: in-cluster config, then the default loading rules)")
	cmd.Flags().StringVar(&opts.nodeName, "node-name", "", "this node's mesh name for Local policy announces (empty: os.Hostname)")
	cmd.Flags().BoolVar(&opts.leaderElect, "leader-elect", true, "leader-elect the allocator via a Lease")
	cmd.Flags().StringVar(&opts.leaderNamespace, "leader-namespace", "", "namespace for the allocator Lease (empty: service account namespace, else kube-system)")
	cmd.Flags().DurationVar(&opts.resync, "resync", 30*time.Second, "informer resync period (drives reconciliation and announce re-checks)")
	cmd.Flags().StringVar(&opts.healthAddr, "health-addr", ":9633", "listen address for the /healthz endpoint (empty disables the health server)")
	cmd.Flags().BoolVar(&opts.speaker, "speaker", true, "run the per-node announcer (prefix files + interface binding)")
	cmd.Flags().BoolVar(&opts.allocator, "allocator", true, "run the LoadBalancer IP allocator (status.loadBalancer.ingress writes)")
	cmd.Flags().StringVar(&opts.logLevel, "log-level", "", "Log level: debug|info|warn|error (default info; env NYLON_LOG_LEVEL; -v is a debug alias)")
	cmd.Flags().BoolVarP(&opts.verbose, "verbose", "v", false, "Verbose output")
	cmd.Flags().BoolVar(&opts.json, "json", false, "Log to stderr as JSON instead of tinted text")

	_ = cmd.MarkFlagRequired("pool")

	return cmd
}

// run wires the whole binary: pool parsing, kube client, informer factory,
// event recorder, health endpoint, speaker, and the leader-elected allocator.
func run(opts *lbOptions) (err error) {
	level, err := logging.Resolve(opts.logLevel, opts.verbose)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	pools, err := ParsePoolSet(opts.pools, opts.excludes)
	if err != nil {
		return fmt.Errorf("invalid pool configuration: %w", err)
	}

	// Node identity must be settled before the logger exists: it labels
	// every record the process ever emits.
	nodeName := opts.nodeName
	if nodeName == "" {
		hostname, err := os.Hostname()
		if err != nil {
			return fmt.Errorf("resolving node name: %w", err)
		}
		nodeName = hostname
	}

	logger, closer, err := logging.New(logging.Config{
		Component: "nylon-lb",
		Node:      nodeName,
		Level:     level,
		JSON:      opts.json,
	})
	if err != nil {
		return fmt.Errorf("building logger: %w", err)
	}
	defer closer()
	// Every failure past this point is a structured error record, not just a
	// cobra stderr line.
	defer func() {
		if err != nil {
			logger.Error("nylon-lb failed", "error", err)
		}
	}()
	// Route client-go's internal logging (informers, leader election) into
	// the same slog pipeline instead of bare stderr.
	klog.SetLogger(logr.FromSlogHandler(logger.Handler()))

	metrics := NewMetrics(nodeName)

	logger.Info("starting nylon-lb",
		"version", buildinfo.Version,
		"commit", buildinfo.Commit,
		"node", nodeName,
		"pools", pools.Names(),
		"lb_class", opts.lbClass,
		"prefixes_dir", opts.prefixesDir,
		"health_addr", opts.healthAddr,
		"leader_elect", opts.leaderElect,
	)

	cfg, err := kubeConfig(opts.kubeconfig)
	if err != nil {
		return err
	}

	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("building kube clientset: %w", err)
	}
	factory := informers.NewSharedInformerFactory(clientset, opts.resync)

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return fmt.Errorf("registering client-go scheme: %w", err)
	}
	broadcaster := record.NewBroadcaster()
	recorder := broadcaster.NewRecorder(scheme, corev1.EventSource{Component: "nylon-lb"})
	broadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: clientset.CoreV1().Events("")})
	defer broadcaster.Shutdown()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// writePrefixFile assumes the dir exists, so create it before the speaker
	// ever runs (the DaemonSet mounts it, but bare-binary runs need this).
	if err := os.MkdirAll(opts.prefixesDir, 0o755); err != nil {
		return fmt.Errorf("creating prefixes dir %s: %w", opts.prefixesDir, err)
	}

	// Both components register their informer handlers at construction, so
	// both are built unconditionally — a disabled component just never runs.
	// Harmless: the handlers only enqueue keys, and Run is what drains.
	controller := NewController(ControllerOptions{
		Client:   clientset,
		Factory:  factory,
		Pools:    pools,
		LBClass:  opts.lbClass,
		Recorder: recorder,
		Logger:   logger.With("module", log.ScopeAllocator),
		Metrics:  metrics,
	})

	var binder AddrBinder
	if opts.bindInterface != "" {
		binder, err = newNetlinkBinder(opts.bindInterface, pools)
		if err != nil {
			return err
		}
	}
	speaker, err := NewSpeaker(SpeakerOptions{
		Client:      clientset,
		Factory:     factory,
		Pools:       pools,
		LBClass:     opts.lbClass,
		PrefixesDir: opts.prefixesDir,
		NodeName:    nodeName,
		Binder:      binder,
		Logger:      logger.With("module", log.ScopeSpeaker),
		Resync:      opts.resync,
		Metrics:     metrics,
	})
	if err != nil {
		return fmt.Errorf("building speaker: %w", err)
	}

	// The health server starts BEFORE cache sync so readiness is actually
	// observable: /healthz answers 200 as soon as the process serves
	// (liveness = process alive), /readyz stays 503 until the informer
	// caches sync (readiness = able to reconcile), and /metrics exposes the
	// metric set.
	var synced atomic.Bool
	var healthSrv *http.Server
	if opts.healthAddr != "" {
		ln, err := net.Listen("tcp", opts.healthAddr)
		if err != nil {
			return fmt.Errorf("listening on health address %s: %w", opts.healthAddr, err)
		}
		mux := http.NewServeMux()
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok\n"))
		})
		mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
			if synced.Load() {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("ok\n"))
				return
			}
			http.Error(w, "informer caches not synced", http.StatusServiceUnavailable)
		})
		mux.Handle("/metrics", metrics.Handler())
		healthSrv = &http.Server{Handler: mux}
		go func() {
			if err := healthSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logger.Error("health server failed", "error", err)
			}
		}()
	}

	factory.Start(ctx.Done())
	for t, ok := range factory.WaitForCacheSync(ctx.Done()) {
		if !ok {
			return fmt.Errorf("informer cache %v failed to sync", t)
		}
	}
	synced.Store(true)

	if opts.speaker {
		// The speaker runs on EVERY replica, never leader-elected: announce
		// placement is per-node state (Cluster policy announces from every
		// node; Local policy needs each node's own endpoint view), and the
		// idempotent file writer + GC make concurrent reconciles converge.
		go func() {
			if err := speaker.Run(ctx); err != nil {
				logger.Error("speaker failed", "error", err)
			}
		}()
	}

	if opts.allocator {
		if opts.leaderElect {
			go runAllocatorElected(ctx, controller, clientset, opts.leaderNamespace, nodeName, logger, metrics)
		} else {
			go func() {
				if err := controller.Run(ctx); err != nil {
					logger.Error("allocator controller failed", "error", err)
				}
			}()
		}
	}

	<-ctx.Done()

	if healthSrv != nil {
		shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelShutdown()
		if err := healthSrv.Shutdown(shutdownCtx); err != nil {
			logger.Warn("health server shutdown", "error", err)
		}
	}

	return nil
}

// runAllocatorElected starts the allocator under a Lease so exactly one
// replica writes Service status at a time. Only the allocator needs this: it
// is the single cluster-wide writer of status.loadBalancer.ingress, and
// concurrent allocation could hand the same IP to two Services before status
// converges. A partitioned ex-leader is still safe — status writes are
// single-object CAS, and on conflict the reconciler re-derives the taken set
// from a re-fetch, so dual leaders converge instead of double-assigning.
func runAllocatorElected(ctx context.Context, controller *Controller, clientset kubernetes.Interface, leaderNamespace, nodeName string, logger *slog.Logger, metrics *Metrics) {
	ns := leaderNamespace
	if ns == "" {
		ns = serviceAccountNamespace()
	}

	identity := fmt.Sprintf("%s-%d", nodeName, os.Getpid())
	lock := &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{Name: leaseName, Namespace: ns},
		Client:    clientset.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: identity,
		},
	}

	elector, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
		Lock:          lock,
		LeaseDuration: 15 * time.Second,
		RenewDeadline: 10 * time.Second,
		RetryPeriod:   2 * time.Second,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(leaderCtx context.Context) {
				if metrics != nil {
					metrics.Leader.Store(1)
				}
				// leaderCtx is cancelled on leadership loss, which unwinds
				// the controller's workqueue loop.
				if err := controller.Run(leaderCtx); err != nil {
					logger.Error("allocator controller failed", "error", err)
				}
			},
			OnStoppedLeading: func() {
				if metrics != nil {
					metrics.Leader.Store(0)
				}
				logger.Warn("lost allocator leadership", "identity", identity)
			},
		},
		Name: leaseName,
	})
	if err != nil {
		logger.Error("building leader elector", "error", err)
		return
	}
	elector.Run(ctx)
}

// serviceAccountNamespace reads the in-cluster namespace file, falling back
// to kube-system on any error (bare-binary runs outside a pod).
func serviceAccountNamespace() string {
	b, err := os.ReadFile(saNamespacePath)
	if err != nil || len(b) == 0 {
		return fallbackNamespace
	}
	return strings.TrimSpace(string(b))
}

// kubeConfig follows the pinned precedence: explicit --kubeconfig, then
// in-cluster config, then the default clientcmd loading rules (KUBECONFIG
// env, ~/.kube/config) for bare-binary runs.
func kubeConfig(path string) (*rest.Config, error) {
	var cfg *rest.Config
	var err error
	if path != "" {
		cfg, err = clientcmd.BuildConfigFromFlags("", path)
	} else {
		cfg, err = rest.InClusterConfig()
		if err != nil {
			cfg, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
				&clientcmd.ClientConfigLoadingRules{},
				&clientcmd.ConfigOverrides{},
			).ClientConfig()
		}
	}
	if err != nil {
		return nil, fmt.Errorf("building kube client config: %w", err)
	}
	return cfg, nil
}
