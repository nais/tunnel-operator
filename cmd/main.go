package main

import (
	"log/slog"
	"os"
	"strings"

	_ "k8s.io/client-go/plugin/pkg/client/auth"

	naisiov1alpha1 "github.com/nais/tunnel-operator/api/v1alpha1"
	"github.com/nais/tunnel-operator/internal/controller"
	"github.com/nais/tunnel-operator/internal/provider"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	crcluster "sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
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

	tenantName := os.Getenv("TENANT_NAME")
	clusterNames := os.Getenv("CLUSTERS")
	staticClustersRaw := os.Getenv("STATIC_CLUSTERS")

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

	if err := (&controller.TunnelReconciler{
		Scheme: scheme,
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

	slog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		slog.Error("problem running manager", "error", err)
		os.Exit(1)
	}
}
