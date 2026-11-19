package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	vmv1 "github.com/thebsdbox/kube-vm-controller/api/v1alpha1"
	"github.com/thebsdbox/kube-vm-controller/internal/controller"
	"github.com/thebsdbox/kube-vm-controller/internal/kubevm"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(vmv1.AddToScheme(scheme))
}

func main() {
	var (
		runScheduler    bool
		runAgent        bool
		nodeName        string
		socket          string
		leaderElect     bool
		leaderElectNS   string
		metricsAddr     string
		probeAddr       string
		syncPeriod      time.Duration
		agentNS         string
		agentSelector   string
		podNetworkImage string
	)
	flag.BoolVar(&runScheduler, "scheduler", false, "Run the scheduler that assigns VirtualMachines to nodes.")
	flag.BoolVar(&runAgent, "agent", false, "Run the node agent that drives the local kube-vm daemon.")
	flag.StringVar(&nodeName, "node-name", os.Getenv("NODE_NAME"), "Name of this node (agent only). Defaults to $NODE_NAME.")
	flag.StringVar(&socket, "kube-vm-socket", kubevm.DefaultSocket, "kube-vm daemon control socket (agent only).")
	flag.BoolVar(&leaderElect, "leader-elect", false, "Enable leader election (scheduler only).")
	flag.StringVar(&leaderElectNS, "leader-election-namespace", "", "Namespace for the leader election lease. Defaults to the in-cluster namespace.")
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Address for the metrics endpoint; 0 disables it.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Address for the health probe endpoint.")
	flag.DurationVar(&syncPeriod, "sync-period", 10*time.Second, "How often the agent polls VM state from the kube-vm daemon.")
	flag.StringVar(&agentNS, "agent-namespace", "kube-vm-system", "Namespace of the agent pods (scheduler only).")
	flag.StringVar(&agentSelector, "agent-selector", "app.kubernetes.io/component=agent", "Label selector for agent pods (scheduler only).")
	flag.StringVar(&podNetworkImage, "pod-network-image", os.Getenv("POD_NETWORK_IMAGE"), "Enable experimental IPv4/Cilium-veth Pod networking using this helper image.")
	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	if err := run(runScheduler, runAgent, nodeName, socket, leaderElect, leaderElectNS, metricsAddr, probeAddr, syncPeriod, agentNS, agentSelector, podNetworkImage); err != nil {
		setupLog.Error(err, "unable to run controller")
		os.Exit(1)
	}
}

func run(runScheduler, runAgent bool, nodeName, socket string, leaderElect bool, leaderElectNS, metricsAddr, probeAddr string,
	syncPeriod time.Duration, agentNS, agentSelector, podNetworkImage string) error {
	if !runScheduler && !runAgent {
		return fmt.Errorf("at least one of --scheduler or --agent is required")
	}
	if runAgent && nodeName == "" {
		return fmt.Errorf("--node-name (or $NODE_NAME) is required with --agent")
	}
	if runAgent && leaderElect {
		return fmt.Errorf("--leader-elect applies to the scheduler only; every node needs its own agent")
	}
	selector, err := labels.Parse(agentSelector)
	if err != nil {
		return fmt.Errorf("invalid --agent-selector: %w", err)
	}

	cacheOpts := cache.Options{}
	if runScheduler && !runAgent {
		// Only the agent pods are needed, not every pod in the cluster.
		cacheOpts.ByObject = map[client.Object]cache.ByObject{
			&corev1.Pod{}: {Namespaces: map[string]cache.Config{agentNS: {}}},
		}
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                  scheme,
		Metrics:                 metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress:  probeAddr,
		LeaderElection:          leaderElect,
		LeaderElectionID:        "kube-vm-controller-scheduler.vm.kube-vm.io",
		LeaderElectionNamespace: leaderElectNS,
		Cache:                   cacheOpts,
		// Provisioning content is read on demand rather than watching every Secret and ConfigMap.
		Client: client.Options{Cache: &client.CacheOptions{DisableFor: []client.Object{&corev1.Secret{}, &corev1.ConfigMap{}}}},
	})
	if err != nil {
		return fmt.Errorf("creating manager: %w", err)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return err
	}
	ready := healthz.Ping
	if runScheduler {
		if err := (&controller.SchedulerReconciler{
			Client: mgr.GetClient(), AgentNamespace: agentNS, AgentSelector: selector,
		}).SetupWithManager(mgr); err != nil {
			return fmt.Errorf("setting up scheduler: %w", err)
		}
	}
	if runAgent {
		daemon := kubevm.NewSocketClient(socket)
		if err := (&controller.AgentReconciler{
			Client: mgr.GetClient(), Scheme: mgr.GetScheme(), Daemon: daemon,
			NodeName: nodeName, SyncPeriod: syncPeriod,
			PodNetworkImage: podNetworkImage, PodNetworkDir: filepath.Join(filepath.Dir(socket), "pod-network"),
		}).SetupWithManager(mgr); err != nil {
			return fmt.Errorf("setting up agent: %w", err)
		}
		// An agent is only useful to the scheduler once it can reach its daemon.
		ready = func(req *http.Request) error {
			ctx, cancel := context.WithTimeout(req.Context(), 3*time.Second)
			defer cancel()
			return daemon.Ping(ctx)
		}
	}
	if err := mgr.AddReadyzCheck("readyz", ready); err != nil {
		return err
	}

	setupLog.Info("starting manager", "scheduler", runScheduler, "agent", runAgent, "node", nodeName)
	return mgr.Start(ctrl.SetupSignalHandler())
}
