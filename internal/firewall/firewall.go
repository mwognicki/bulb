// Package firewall implements the bulb firewall-agent. In this Phase 2
// slice it watches LBPort CRs, computes the desired per-node exposed
// port set, and surfaces that state without mutating any host firewall.
package firewall

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"

	bulbv1alpha1 "github.com/mwognicki/bulb/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

const allNodesToken = "*"

// Run is the `bulb firewall-agent` subcommand entrypoint.
func Run(args []string) error {
	fs := flag.NewFlagSet("firewall-agent", flag.ContinueOnError)
	nodeName := fs.String("node-name", os.Getenv("NODE_NAME"), "Kubernetes node name this agent instance is responsible for")
	metricsAddr := fs.String("metrics-bind-address", ":9100", "address the metrics endpoint binds to")
	probeAddr := fs.String("health-probe-bind-address", ":8081", "address the readiness/liveness probe binds to")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *nodeName == "" {
		return fmt.Errorf("--node-name is required (or set NODE_NAME)")
	}

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(bulbv1alpha1.AddToScheme(scheme))

	ctrl.SetLogger(zap.New(zap.UseDevMode(false)))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: *metricsAddr},
		HealthProbeBindAddress: *probeAddr,
		LeaderElection:         false,
	})
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}

	r := &AgentReconciler{
		Client:   mgr.GetClient(),
		NodeName: *nodeName,
	}
	if err := r.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup firewall-agent reconciler: %w", err)
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

type AgentReconciler struct {
	client.Client
	NodeName string

	mu          sync.Mutex
	lastDesired []PortSpec
}

type PortSpec struct {
	Port     int32
	Protocol corev1.Protocol
}

func (p PortSpec) String() string {
	return fmt.Sprintf("%d/%s", p.Port, strings.ToLower(string(p.Protocol)))
}

func (r *AgentReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	logger := ctrl.LoggerFrom(ctx).WithValues("node", r.NodeName)

	var lbports bulbv1alpha1.LBPortList
	if err := r.List(ctx, &lbports); err != nil {
		return ctrl.Result{}, fmt.Errorf("list lbports: %w", err)
	}

	desired := DesiredPortsForNode(lbports.Items, r.NodeName)
	if r.setLastDesired(desired) {
		logger.Info("desired exposure set changed", "ports", formatPorts(desired))
	}
	return ctrl.Result{}, nil
}

func (r *AgentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.NodeName == "" {
		return fmt.Errorf("AgentReconciler.NodeName is required")
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&bulbv1alpha1.LBPort{}).
		Complete(r)
}

func DesiredPortsForNode(lbports []bulbv1alpha1.LBPort, nodeName string) []PortSpec {
	seen := make(map[PortSpec]struct{}, len(lbports))
	for _, lbport := range lbports {
		if !appliesToNode(lbport.Spec.Nodes, nodeName) {
			continue
		}
		protocol := lbport.Spec.Protocol
		if protocol == "" {
			protocol = corev1.ProtocolTCP
		}
		seen[PortSpec{Port: lbport.Spec.Port, Protocol: protocol}] = struct{}{}
	}

	out := make([]PortSpec, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Port != out[j].Port {
			return out[i].Port < out[j].Port
		}
		return out[i].Protocol < out[j].Protocol
	})
	return out
}

func appliesToNode(nodes []string, nodeName string) bool {
	for _, node := range nodes {
		if node == allNodesToken || node == nodeName {
			return true
		}
	}
	return false
}

func formatPorts(ports []PortSpec) []string {
	formatted := make([]string, 0, len(ports))
	for _, port := range ports {
		formatted = append(formatted, port.String())
	}
	return formatted
}

func (r *AgentReconciler) setLastDesired(next []PortSpec) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	if slices.Equal(r.lastDesired, next) {
		return false
	}
	r.lastDesired = append([]PortSpec(nil), next...)
	return true
}

func (r *AgentReconciler) LastDesired() []PortSpec {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]PortSpec(nil), r.lastDesired...)
}

func healthz(_ *http.Request) error { return nil }
