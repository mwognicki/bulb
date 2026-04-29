package controller

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

var registerMetrics sync.Once

var (
	reconcileTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "bulb_controller_reconcile_total",
			Help: "Number of Service reconciliations by result.",
		},
		[]string{"result"},
	)
	reconcileDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "bulb_controller_reconcile_duration_seconds",
			Help:    "Wall-clock time spent in a single Service reconciliation, by result.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"result"},
	)
)

func initMetrics() {
	registerMetrics.Do(func() {
		ctrlmetrics.Registry.MustRegister(reconcileTotal, reconcileDuration)
	})
}
