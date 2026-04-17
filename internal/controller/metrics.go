package controller

import (
	"github.com/prometheus/client_golang/prometheus"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	reconciliationsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "tunnel_operator_reconciliations_total",
		Help: "Total number of tunnel reconciliations.",
	}, []string{"result"})

	tunnelsActive = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "tunnel_operator_tunnels_active",
		Help: "Number of tunnels by phase.",
	}, []string{"phase", "namespace"})

	portAllocationsActive = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "tunnel_operator_port_allocations_active",
		Help: "Number of currently allocated forwarder ports.",
	})
)

func init() {
	crmetrics.Registry.MustRegister(
		reconciliationsTotal,
		tunnelsActive,
		portAllocationsActive,
	)
}
