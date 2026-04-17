package main

import (
	"context"
	"log/slog"
	"os"
	"strings"

	"github.com/go-logr/logr"
	operatorgrpc "github.com/nais/tunnel-operator/internal/grpc"
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	naisiov1alpha1 "github.com/nais/tunnel-operator/api/v1alpha1"
	"github.com/nais/tunnel-operator/internal/controller"
	"github.com/nais/tunnel-operator/internal/provider"
	"github.com/nais/tunnel-operator/pkg/portalloc"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crcluster "sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	crlog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	//+kubebuilder:scaffold:imports
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(naisiov1alpha1.AddToScheme(scheme))
	//+kubebuilder:scaffold:scheme
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	crlog.SetLogger(logr.FromSlogHandler(slog.Default().Handler()))

	tenantName := os.Getenv("TENANT_NAME")
	clusterNames := os.Getenv("CLUSTERS")
	staticClustersRaw := os.Getenv("STATIC_CLUSTERS")
	grpcAddr := os.Getenv("GRPC_ADDR")
	if grpcAddr == "" {
		grpcAddr = ":9090"
	}
	forwarderServiceName := os.Getenv("FORWARDER_SERVICE_NAME")
	podNamespace := os.Getenv("POD_NAMESPACE")

	var clusters []string
	if clusterNames != "" {
		clusters = strings.Split(clusterNames, ",")
	}

	staticClusters, err := provider.ParseStaticClusters(staticClustersRaw)
	if err != nil {
		slog.Error("unable to parse static clusters", "error", err)
		os.Exit(1)
	}

	clusterConfigs, err := provider.CreateClusterConfigMap(tenantName, clusters, staticClusters)
	if err != nil {
		slog.Error("unable to create cluster configs", "error", err)
		os.Exit(1)
	}

	slog.Info("configured clusters", "count", len(clusterConfigs))

	staticProvider := provider.NewStatic(clusterConfigs, func(o *crcluster.Options) {
		o.Scheme = scheme
	})

	mgr, err := mcmanager.New(ctrl.GetConfigOrDie(), staticProvider, manager.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: ":8080",
		},
		HealthProbeBindAddress: ":8081",
		LeaderElection:         false,
		LeaderElectionID:       "tunnel-operator.nais.io",
	})
	if err != nil {
		slog.Error("unable to create manager", "error", err)
		os.Exit(1)
	}
	localMgr := mgr.GetLocalManager()

	allocator := portalloc.New(10000, 60000)
	forwarderServiceKey := client.ObjectKey{Name: forwarderServiceName, Namespace: podNamespace}
	grpcServer := operatorgrpc.NewForwarderServer(localMgr.GetClient(), allocator, forwarderServiceKey)
	loadExistingAllocations(context.Background(), localMgr.GetAPIReader(), allocator)

	if err := (&controller.TunnelReconciler{
		Scheme:              scheme,
		PortAllocator:       allocator,
		ForwarderServer:     grpcServer,
		LocalClient:         localMgr.GetClient(),
		ForwarderServiceKey: forwarderServiceKey,
	}).SetupWithManager(mgr); err != nil {
		slog.Error("unable to create controller", "controller", "Tunnel", "error", err)
		os.Exit(1)
	}
	//+kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		slog.Error("unable to set up health check", "error", err)
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		slog.Error("unable to set up ready check", "error", err)
		os.Exit(1)
	}

	ctx := ctrl.SetupSignalHandler()
	go func() {
		if err := grpcServer.Start(ctx, grpcAddr); err != nil {
			slog.Error("problem running grpc server", "error", err, "addr", grpcAddr)
		}
	}()

	slog.Info("starting manager", "grpc_addr", grpcAddr)
	if err := mgr.Start(ctx); err != nil {
		slog.Error("problem running manager", "error", err)
		os.Exit(1)
	}
}

func loadExistingAllocations(ctx context.Context, kubeClient client.Reader, allocator *portalloc.PortAllocator) {
	tunnels := &naisiov1alpha1.TunnelList{}
	if err := kubeClient.List(ctx, tunnels); err != nil {
		slog.Warn("unable to restore existing forwarder ports", "error", err)
		return
	}

	for i := range tunnels.Items {
		tunnel := &tunnels.Items[i]
		if tunnel.Status.ForwarderPort <= 0 {
			continue
		}

		allocator.LoadExisting(tunnel.Namespace+"/"+tunnel.Name, tunnel.Status.ForwarderPort)
	}
}
