// Package controller implements the bulb Service reconciler.
// It watches Services with type=LoadBalancer (loadBalancerClass empty
// or "bulb"), creates per-Service proxy DaemonSets, and emits LBPort
// and DNSRecord CRs.
package controller

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os/signal"
	"syscall"

	bulbv1alpha1 "github.com/mwognicki/bulb/api/v1alpha1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// Run is the `bulb controller` subcommand entrypoint.
func Run(args []string) error {
	fs := flag.NewFlagSet("controller", flag.ContinueOnError)
	namespace := fs.String("namespace", "bulb-system", "namespace where bulb workloads are created")
	image := fs.String("image", "", "bulb container image used by per-Service proxy DaemonSets (required)")
	nodeIPsCM := fs.String("node-ips-configmap", "node-ips", "ConfigMap (in --namespace) mapping node-name → public IPv4")
	metricsAddr := fs.String("metrics-bind-address", ":9100", "address the metrics endpoint binds to")
	probeAddr := fs.String("health-probe-bind-address", ":8081", "address the readiness/liveness probe binds to")
	leaderID := fs.String("leader-election-id", "bulb-controller", "lease name used for leader election")
	leaderEnable := fs.Bool("leader-elect", true, "enable leader election")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *image == "" {
		return fmt.Errorf("--image is required")
	}

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(bulbv1alpha1.AddToScheme(scheme))

	ctrl.SetLogger(zap.New(zap.UseDevMode(false)))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                  scheme,
		Metrics:                 metricsserver.Options{BindAddress: *metricsAddr},
		HealthProbeBindAddress:  *probeAddr,
		LeaderElection:          *leaderEnable,
		LeaderElectionID:        *leaderID,
		LeaderElectionNamespace: *namespace,
	})
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}

	r := &ServiceReconciler{
		Client:           mgr.GetClient(),
		Scheme:           mgr.GetScheme(),
		EventRecorder:    mgr.GetEventRecorderFor("bulb-controller"),
		Namespace:        *namespace,
		Image:            *image,
		NodeIPsConfigMap: *nodeIPsCM,
	}
	if err := r.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup reconciler: %w", err)
	}

	if err := mgr.AddHealthzCheck("ping", healthz); err != nil {
		return fmt.Errorf("add healthz: %w", err)
	}
	if err := mgr.AddReadyzCheck("ping", healthz); err != nil {
		return fmt.Errorf("add readyz: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := mgr.Start(ctx); err != nil {
		return fmt.Errorf("manager exited: %w", err)
	}
	return nil
}

func healthz(_ *http.Request) error { return nil }
