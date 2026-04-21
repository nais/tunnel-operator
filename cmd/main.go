package main

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/go-logr/logr"
	operatorgrpc "github.com/nais/tunnel-operator/internal/grpc"
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	naisiov1alpha1 "github.com/nais/tunnel-operator/api/v1alpha1"
	"github.com/nais/tunnel-operator/internal/controller"
	"github.com/nais/tunnel-operator/pkg/portalloc"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	crlog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	//+kubebuilder:scaffold:imports
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(naisiov1alpha1.AddToScheme(scheme))
	//+kubebuilder:scaffold:scheme
}

func main() {
	logLevel := parseLogLevel(os.Getenv("LOG_LEVEL"))
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel})))
	crlog.SetLogger(logr.FromSlogHandler(slog.Default().Handler()))

	grpcAddr := os.Getenv("GRPC_ADDR")
	if grpcAddr == "" {
		grpcAddr = ":9090"
	}
	forwarderServiceName := os.Getenv("FORWARDER_SERVICE_NAME")
	podNamespace := os.Getenv("POD_NAMESPACE")

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), manager.Options{
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

	allocator := portalloc.New(envInt32("FORWARDER_PORT_RANGE_MIN", 51820), envInt32("FORWARDER_PORT_RANGE_MAX", 51919))
	forwarderServiceKey := client.ObjectKey{Name: forwarderServiceName, Namespace: podNamespace}
	grpcServer := operatorgrpc.NewForwarderServer(mgr.GetClient(), allocator, forwarderServiceKey)
	loadExistingAllocations(context.Background(), mgr.GetAPIReader(), allocator)

	if err := (&controller.TunnelReconciler{
		Client:              mgr.GetClient(),
		Scheme:              scheme,
		PortAllocator:       allocator,
		ForwarderServer:     grpcServer,
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

	if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		return grpcServer.Start(ctx, grpcAddr)
	})); err != nil {
		slog.Error("unable to add grpc server to manager", "error", err)
		os.Exit(1)
	}

	slog.Info("starting manager", "grpc_addr", grpcAddr)
	ctx := ctrl.SetupSignalHandler()
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

func envInt32(key string, fallback int32) int32 {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.ParseInt(v, 10, 32)
	if err != nil {
		slog.Warn("invalid env var, using default", "key", key, "value", v, "default", fallback)
		return fallback
	}
	return int32(n)
}

func parseLogLevel(raw string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
