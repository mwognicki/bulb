package controller

import (
	"errors"
	"fmt"
	"net"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// Annotation keys; mirror the table in CLAUDE.md.
const (
	AnnotationNodes               = "bulb.toturi.tech/nodes"
	AnnotationAllowPrivilegedPort = "bulb.toturi.tech/allow-privileged-port"

	labelManagedBy   = "app.kubernetes.io/managed-by"
	labelManagedByV  = "bulb"
	labelService     = "bulb.toturi.tech/service"
	labelServiceNs   = "bulb.toturi.tech/service-namespace"
	containerName    = "proxy"
	defaultDrainTime = "30s"
)

// ErrPrivilegedPortDenied is returned when a Service requests a port < 1024
// without the bulb.toturi.tech/allow-privileged-port="true" opt-in annotation.
var ErrPrivilegedPortDenied = errors.New("port < 1024 requires bulb.toturi.tech/allow-privileged-port=\"true\"")

// BuildDaemonSet renders the per-Service proxy DaemonSet. It does not
// touch the API server — it returns the desired object so callers can
// Create/Update or diff it as they like.
//
// @adr OwnerReferences are deliberately *not* used to track the
// DS↔Service relationship. The Service lives in the user's namespace
// (e.g. "demo") while the DS lives in bulb-system; Kubernetes forbids
// cross-namespace OwnerReferences for namespaced kinds, so cluster-side
// GC can't help us here. Instead we tag the DS with stable labels
// (labelService, labelServiceNs, labelManagedBy) and the reconciler
// performs explicit cleanup on Service delete or type change. Revisit
// only if bulb ever moves to one DS per namespace alongside the
// Service, in which case OwnerReferences become viable.
func BuildDaemonSet(svc *corev1.Service, image, namespace string) (*appsv1.DaemonSet, error) {
	if svc == nil {
		return nil, errors.New("service is nil")
	}
	if v4, v6 := splitClusterIPs(svc); v4 == "" && v6 == "" {
		return nil, fmt.Errorf("service %s/%s has no ClusterIP", svc.Namespace, svc.Name)
	}
	if len(svc.Spec.Ports) == 0 {
		return nil, fmt.Errorf("service %s/%s has no ports", svc.Namespace, svc.Name)
	}

	allowPrivileged := svc.Annotations[AnnotationAllowPrivilegedPort] == "true"
	for _, p := range svc.Spec.Ports {
		if p.Port < 1024 && !allowPrivileged {
			return nil, fmt.Errorf("%w: port %d on service %s/%s", ErrPrivilegedPortDenied, p.Port, svc.Namespace, svc.Name)
		}
	}

	ports, args, err := portsAndArgs(svc)
	if err != nil {
		return nil, err
	}

	nodeSelector, nodeAffinity, err := nodePlacement(svc.Annotations[AnnotationNodes])
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", AnnotationNodes, err)
	}

	labels := serviceLabels(svc)

	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      DaemonSetName(svc),
			Namespace: namespace,
			Labels:    labels,
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					HostNetwork:                   false,
					DNSPolicy:                     corev1.DNSClusterFirstWithHostNet,
					TerminationGracePeriodSeconds: ptr[int64](30),
					NodeSelector:                  nodeSelector,
					Affinity:                      nodeAffinity,
					Containers: []corev1.Container{{
						Name:            containerName,
						Image:           image,
						ImagePullPolicy: corev1.PullIfNotPresent,
						Args:            append([]string{"proxy", "--drain-timeout=" + defaultDrainTime}, args...),
						Ports:           ports,
						SecurityContext: hardenedSecurityContext(),
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("10m"),
								corev1.ResourceMemory: resource.MustParse("32Mi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceMemory: resource.MustParse("128Mi"),
							},
						},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(svc.Spec.Ports[0].Port)},
							},
							PeriodSeconds: 5,
						},
					}},
				},
			},
		},
	}
	return ds, nil
}

// DaemonSetName returns the deterministic per-Service DS name.
func DaemonSetName(svc *corev1.Service) string {
	return fmt.Sprintf("bulb-%s-%s", svc.Namespace, svc.Name)
}

func portsAndArgs(svc *corev1.Service) ([]corev1.ContainerPort, []string, error) {
	v4ClusterIP, v6ClusterIP := splitClusterIPs(svc)

	ports := make([]corev1.ContainerPort, 0, len(svc.Spec.Ports))
	args := make([]string, 0, len(svc.Spec.Ports))
	for _, p := range svc.Spec.Ports {
		if p.Protocol != "" && p.Protocol != corev1.ProtocolTCP {
			return nil, nil, fmt.Errorf("port %d: only TCP supported in Phase 1, got %s", p.Port, p.Protocol)
		}
		target := p.TargetPort.String()
		if target == "" || target == "0" {
			target = fmt.Sprintf("%d", p.Port)
		}
		ports = append(ports, corev1.ContainerPort{
			Name:          portName(p),
			ContainerPort: p.Port,
			HostPort:      p.Port,
			Protocol:      corev1.ProtocolTCP,
		})
		if v4ClusterIP != "" {
			args = append(args, fmt.Sprintf("--upstream=0.0.0.0:%d=%s:%s", p.Port, v4ClusterIP, target))
		}
		if v6ClusterIP != "" {
			args = append(args, fmt.Sprintf("--upstream=[::]:%d=[%s]:%s", p.Port, v6ClusterIP, target))
		}
	}
	return ports, args, nil
}

// splitClusterIPs returns the IPv4 and IPv6 ClusterIPs for a Service.
// It prefers Spec.ClusterIPs (dual-stack); falls back to Spec.ClusterIP.
// Either return value may be empty if the Service is single-stack.
func splitClusterIPs(svc *corev1.Service) (v4, v6 string) {
	candidates := svc.Spec.ClusterIPs
	if len(candidates) == 0 && svc.Spec.ClusterIP != "" {
		candidates = []string{svc.Spec.ClusterIP}
	}
	for _, raw := range candidates {
		if raw == "" || raw == corev1.ClusterIPNone {
			continue
		}
		ip := net.ParseIP(raw)
		if ip == nil {
			continue
		}
		if ip.To4() != nil {
			if v4 == "" {
				v4 = raw
			}
		} else {
			if v6 == "" {
				v6 = raw
			}
		}
	}
	return v4, v6
}

func portName(p corev1.ServicePort) string {
	if p.Name != "" {
		return p.Name
	}
	return fmt.Sprintf("p-%d", p.Port)
}

// nodePlacement parses bulb.toturi.tech/nodes as a Kubernetes label selector
// (e.g. "role=edge", "role in (edge,gateway)", "!cordoned") and
// translates it into a (nodeSelector, nodeAffinity) pair.
//
// matchLabels go to nodeSelector (equality) because that's the cheapest
// scheduler path; matchExpressions need nodeAffinity because
// nodeSelector can't express set-based operations.
//
// Empty input → (nil, nil): DaemonSet schedules on every node.
func nodePlacement(raw string) (map[string]string, *corev1.Affinity, error) {
	if raw == "" {
		return nil, nil, nil
	}
	sel, err := metav1.ParseToLabelSelector(raw)
	if err != nil {
		return nil, nil, err
	}

	var nodeSelector map[string]string
	if len(sel.MatchLabels) > 0 {
		nodeSelector = sel.MatchLabels
	}

	if len(sel.MatchExpressions) == 0 {
		return nodeSelector, nil, nil
	}

	exprs := make([]corev1.NodeSelectorRequirement, 0, len(sel.MatchExpressions))
	for _, e := range sel.MatchExpressions {
		op, err := toNodeSelectorOperator(e.Operator)
		if err != nil {
			return nil, nil, err
		}
		exprs = append(exprs, corev1.NodeSelectorRequirement{
			Key:      e.Key,
			Operator: op,
			Values:   e.Values,
		})
	}
	affinity := &corev1.Affinity{
		NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: exprs}},
			},
		},
	}
	return nodeSelector, affinity, nil
}

func toNodeSelectorOperator(op metav1.LabelSelectorOperator) (corev1.NodeSelectorOperator, error) {
	switch op {
	case metav1.LabelSelectorOpIn:
		return corev1.NodeSelectorOpIn, nil
	case metav1.LabelSelectorOpNotIn:
		return corev1.NodeSelectorOpNotIn, nil
	case metav1.LabelSelectorOpExists:
		return corev1.NodeSelectorOpExists, nil
	case metav1.LabelSelectorOpDoesNotExist:
		return corev1.NodeSelectorOpDoesNotExist, nil
	default:
		return "", fmt.Errorf("unsupported selector operator %q", op)
	}
}

func hardenedSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: ptr(false),
		Privileged:               ptr(false),
		ReadOnlyRootFilesystem:   ptr(true),
		RunAsNonRoot:             ptr(true),
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
		},
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

func ptr[T any](v T) *T { return &v }
