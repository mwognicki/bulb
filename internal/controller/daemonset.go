package controller

import (
	"errors"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// Annotation keys; mirror the table in CLAUDE.md.
const (
	AnnotationNodes               = "bulb.io/nodes"
	AnnotationAllowPrivilegedPort = "bulb.io/allow-privileged-port"

	labelManagedBy   = "app.kubernetes.io/managed-by"
	labelManagedByV  = "bulb"
	labelService     = "bulb.io/service"
	labelServiceNs   = "bulb.io/service-namespace"
	containerName    = "proxy"
	defaultDrainTime = "30s"
)

// ErrPrivilegedPortDenied is returned when a Service requests a port < 1024
// without the bulb.io/allow-privileged-port="true" opt-in annotation.
var ErrPrivilegedPortDenied = errors.New("port < 1024 requires bulb.io/allow-privileged-port=\"true\"")

// BuildDaemonSet renders the per-Service proxy DaemonSet. It does not
// touch the API server — it returns the desired object so callers can
// Create/Update or diff it as they like.
//
// Caller must set OwnerReferences (typically pointing at the Service)
// after this returns; we don't take a runtime.Scheme here.
func BuildDaemonSet(svc *corev1.Service, image, namespace string) (*appsv1.DaemonSet, error) {
	if svc == nil {
		return nil, errors.New("service is nil")
	}
	if svc.Spec.ClusterIP == "" || svc.Spec.ClusterIP == corev1.ClusterIPNone {
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

	labels := map[string]string{
		labelManagedBy: labelManagedByV,
		labelService:   svc.Name,
		labelServiceNs: svc.Namespace,
	}

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
					NodeSelector:                  parseNodeSelector(svc.Annotations[AnnotationNodes]),
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
		args = append(args, fmt.Sprintf("--upstream=0.0.0.0:%d=%s:%s", p.Port, svc.Spec.ClusterIP, target))
	}
	return ports, args, nil
}

func portName(p corev1.ServicePort) string {
	if p.Name != "" {
		return p.Name
	}
	return fmt.Sprintf("p-%d", p.Port)
}

// parseNodeSelector turns a comma-separated key=value list into a map.
// Empty input → nil (DaemonSet schedules everywhere).
func parseNodeSelector(raw string) map[string]string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	out := map[string]string{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	if len(out) == 0 {
		return nil
	}
	return out
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
