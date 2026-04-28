package firewall

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

var registerMetrics sync.Once

var (
	reconcileTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "bulb_firewall_reconcile_total",
			Help: "Number of firewall-agent reconciliations by result.",
		},
		[]string{"backend", "result"},
	)
	desiredPortsGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "bulb_firewall_desired_ports",
			Help: "Desired firewall port counts by node, backend, and stage.",
		},
		[]string{"node", "backend", "stage"},
	)
	filteredPortsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "bulb_firewall_filtered_ports_total",
			Help: "Number of desired ports filtered out by policy reason.",
		},
		[]string{"backend", "reason"},
	)
)

func initMetrics() {
	registerMetrics.Do(func() {
		ctrlmetrics.Registry.MustRegister(reconcileTotal, desiredPortsGauge, filteredPortsTotal)
	})
}
