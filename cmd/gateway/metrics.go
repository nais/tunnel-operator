package main

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

type gatewayMetrics struct {
	tcpConnectionsTotal  prometheus.Counter
	tcpConnectionsActive prometheus.Gauge
	tcpReceivedBytes     prometheus.Counter
	tcpSentBytes         prometheus.Counter
}

func newGatewayMetrics(tunnelName, tunnelNamespace, targetHost, targetPort string) *gatewayMetrics {
	constLabels := prometheus.Labels{
		"tunnel_name":      tunnelName,
		"tunnel_namespace": tunnelNamespace,
		"target_host":      targetHost,
		"target_port":      targetPort,
	}
	return &gatewayMetrics{
		tcpConnectionsTotal: promauto.NewCounter(prometheus.CounterOpts{
			Name:        "tunnel_gateway_tcp_connections_total",
			Help:        "Total number of TCP connections handled by the gateway.",
			ConstLabels: constLabels,
		}),
		tcpConnectionsActive: promauto.NewGauge(prometheus.GaugeOpts{
			Name:        "tunnel_gateway_tcp_connections_active",
			Help:        "Number of currently active TCP connections.",
			ConstLabels: constLabels,
		}),
		tcpReceivedBytes: promauto.NewCounter(prometheus.CounterOpts{
			Name:        "tunnel_gateway_tcp_received_bytes_total",
			Help:        "Total bytes received from WireGuard tunnel (client to target direction).",
			ConstLabels: constLabels,
		}),
		tcpSentBytes: promauto.NewCounter(prometheus.CounterOpts{
			Name:        "tunnel_gateway_tcp_sent_bytes_total",
			Help:        "Total bytes sent back through WireGuard tunnel (target to client direction).",
			ConstLabels: constLabels,
		}),
	}
}
