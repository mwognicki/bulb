// Package firewall implements the bulb firewall-agent. It watches
// LBPort CRs, computes the desired per-node exposed port set, applies
// policy filtering, and delegates actual mutation to a pluggable
// backend. The first concrete backend is firewalld via D-Bus.
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
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

const allNodesToken = "*"

// Run is the `bulb firewall-agent` subcommand entrypoint.
func Run(args []string) error {
	fs := flag.NewFlagSet("firewall-agent", flag.ContinueOnError)
	nodeName := fs.String("node-name", os.Getenv("NODE_NAME"), "Kubernetes node name this agent instance is responsible for")
	configNamespace := fs.String("config-namespace", defaultConfigNamespace, "namespace of the firewall-agent ConfigMap")
	configName := fs.String("configmap", defaultConfigMapName, "name of the firewall-agent ConfigMap")
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
	initMetrics()

	ctrl.SetLogger(zap.New(zap.UseDevMode(false)))

	restConfig := config.GetConfigOrDie()
	bootstrapClient, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("create bootstrap client: %w", err)
	}
	cfg, err := LoadConfig(context.Background(), bootstrapClient, *configNamespace, *configName)
	if err != nil {
		return err
	}
	backend, err := NewBackend(cfg.Backend, BackendOptions{
		Zone:      cfg.Zone,
		StateFile: cfg.StateFile,
	})
	if err != nil {
		return err
	}

	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
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
		Backend:  backend,
		Policy:   FirewallPolicy{DeniedPorts: cfg.DeniedPorts},
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
	Backend  Backend
	Policy   FirewallPolicy

	mu          sync.Mutex
	lastDesired []PortSpec
}

type PortSpec struct {
	Port            int32
	Protocol        corev1.Protocol
	AllowPrivileged bool
}

func (p PortSpec) String() string {
	return fmt.Sprintf("%d/%s", p.Port, strings.ToLower(string(p.Protocol)))
}

func (p PortSpec) key() PortKey {
	return PortKey{Port: p.Port, Protocol: protocolString(p.Protocol)}
}

func (r *AgentReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	if r.Backend == nil {
		return ctrl.Result{}, fmt.Errorf("firewall backend is required")
	}
	logger := ctrl.LoggerFrom(ctx).WithValues(
		"node", r.NodeName,
		"backend", r.Backend.Name(),
	)

	var lbports bulbv1alpha1.LBPortList
	if err := r.List(ctx, &lbports); err != nil {
		reconcileTotal.WithLabelValues(r.Backend.Name(), "error").Inc()
		return ctrl.Result{}, fmt.Errorf("list lbports: %w", err)
	}

	rawDesired := DesiredPortsForNode(lbports.Items, r.NodeName)
	filtered, rejected := r.Policy.Filter(rawDesired)
	desiredPortsGauge.WithLabelValues(r.NodeName, r.Backend.Name(), "raw").Set(float64(len(rawDesired)))
	desiredPortsGauge.WithLabelValues(r.NodeName, r.Backend.Name(), "filtered").Set(float64(len(filtered)))
	for _, reject := range rejected {
		filteredPortsTotal.WithLabelValues(r.Backend.Name(), reject.Reason).Inc()
	}
	if r.setLastDesired(filtered) {
		logger.Info(
			"desired exposure set changed",
			"raw_ports", formatPorts(rawDesired),
			"filtered_ports", formatPorts(filtered),
			"rejected", formatRejected(rejected),
		)
	}
	if err := r.Backend.Apply(ctx, filtered); err != nil {
		reconcileTotal.WithLabelValues(r.Backend.Name(), "error").Inc()
		logger.Error(err, "backend apply failed", "filtered_ports", formatPorts(filtered))
		return ctrl.Result{}, fmt.Errorf("apply firewall backend %s: %w", r.Backend.Name(), err)
	}
	reconcileTotal.WithLabelValues(r.Backend.Name(), "success").Inc()
	logger.Info("backend apply succeeded", "port_count", len(filtered))
	return ctrl.Result{}, nil
}

func (r *AgentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.NodeName == "" {
		return fmt.Errorf("AgentReconciler.NodeName is required")
	}
	if r.Backend == nil {
		return fmt.Errorf("AgentReconciler.Backend is required")
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&bulbv1alpha1.LBPort{}).
		Complete(r)
}

func DesiredPortsForNode(lbports []bulbv1alpha1.LBPort, nodeName string) []PortSpec {
	seen := make(map[PortKey]PortSpec, len(lbports))
	for _, lbport := range lbports {
		if !appliesToNode(lbport.Spec.Nodes, nodeName) {
			continue
		}
		protocol := lbport.Spec.Protocol
		if protocol == "" {
			protocol = corev1.ProtocolTCP
		}
		key := PortKey{Port: lbport.Spec.Port, Protocol: protocolString(protocol)}
		current := PortSpec{
			Port:            lbport.Spec.Port,
			Protocol:        protocol,
			AllowPrivileged: lbport.Spec.AllowPrivileged,
		}
		if existing, ok := seen[key]; ok && existing.AllowPrivileged {
			current.AllowPrivileged = true
		}
		seen[key] = current
	}

	out := make([]PortSpec, 0, len(seen))
	for _, p := range seen {
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

type FirewallPolicy struct {
	DeniedPorts []int32
}

type RejectedPort struct {
	PortSpec
	Reason string
}

func (p FirewallPolicy) Filter(desired []PortSpec) ([]PortSpec, []RejectedPort) {
	denied := make(map[int32]struct{}, len(p.DeniedPorts))
	for _, port := range p.DeniedPorts {
		denied[port] = struct{}{}
	}

	out := make([]PortSpec, 0, len(desired))
	rejected := make([]RejectedPort, 0)
	for _, spec := range desired {
		if _, blocked := denied[spec.Port]; blocked {
			rejected = append(rejected, RejectedPort{PortSpec: spec, Reason: "denylist"})
			continue
		}
		if spec.Port < 1024 && !spec.AllowPrivileged {
			rejected = append(rejected, RejectedPort{PortSpec: spec, Reason: "privileged"})
			continue
		}
		out = append(out, spec)
	}
	return out, rejected
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

func formatRejected(ports []RejectedPort) []string {
	formatted := make([]string, 0, len(ports))
	for _, port := range ports {
		formatted = append(formatted, fmt.Sprintf("%s:%s", port.String(), port.Reason))
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
