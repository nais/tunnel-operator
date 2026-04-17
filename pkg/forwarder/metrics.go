package forwarder

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	PacketsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "tunnel_forwarder_packets_total",
		Help: "Total number of packets forwarded",
	}, []string{"tunnel_name", "tunnel_namespace", "direction"})

	BytesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "tunnel_forwarder_bytes_total",
		Help: "Total number of bytes forwarded",
	}, []string{"tunnel_name", "tunnel_namespace", "direction"})

	ActiveSessions = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "tunnel_forwarder_active_sessions",
		Help: "Number of active UDP sessions",
	}, []string{"tunnel_name", "tunnel_namespace"})
)

const (
	DirectionClientToGateway = "client_to_gateway"
	DirectionGatewayToClient = "gateway_to_client"
)
