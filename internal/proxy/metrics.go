package proxy

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

var registerProxyMetrics sync.Once

var (
	tcpActiveConnections = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "bulb_proxy_tcp_active_connections",
			Help: "Active TCP connections handled by the proxy.",
		},
		[]string{"listen"},
	)
	upstreamDialFailures = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "bulb_proxy_upstream_dial_failures_total",
			Help: "Total upstream dial failures by protocol and listen address.",
		},
		[]string{"protocol", "listen"},
	)
	forwardedBytes = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "bulb_proxy_forwarded_bytes_total",
			Help: "Total forwarded bytes by protocol, direction, and listener.",
		},
		[]string{"protocol", "direction", "listen"},
	)
	udpActiveSessions = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "bulb_proxy_udp_active_sessions",
			Help: "Active UDP sessions keyed by client source.",
		},
		[]string{"listen"},
	)
	udpPackets = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "bulb_proxy_udp_packets_total",
			Help: "UDP packets forwarded by direction and listener.",
		},
		[]string{"direction", "listen"},
	)
)

func initMetrics() {
	registerProxyMetrics.Do(func() {
		prometheus.MustRegister(
			tcpActiveConnections,
			upstreamDialFailures,
			forwardedBytes,
			udpActiveSessions,
			udpPackets,
		)
	})
}
