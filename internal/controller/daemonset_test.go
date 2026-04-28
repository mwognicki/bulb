package controller

import (
	"errors"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func mkSvc(mods ...func(*corev1.Service)) *corev1.Service {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "demo", Name: "echo"},
		Spec: corev1.ServiceSpec{
			Type:      corev1.ServiceTypeLoadBalancer,
			ClusterIP: "10.96.1.5",
			Ports: []corev1.ServicePort{{
				Name: "https", Port: 8443, TargetPort: intstr.FromInt32(8443), Protocol: corev1.ProtocolTCP,
			}},
		},
	}
	for _, m := range mods {
		m(svc)
	}
	return svc
}

func TestBuildDaemonSet_BasicShape(t *testing.T) {
	ds, err := BuildDaemonSet(mkSvc(), "ghcr.io/mwognicki/bulb:test", "bulb-system")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, want := ds.Name, "bulb-demo-echo"; got != want {
		t.Fatalf("name: got %q want %q", got, want)
	}
	if ds.Namespace != "bulb-system" {
		t.Fatalf("namespace: got %q want bulb-system", ds.Namespace)
	}
	if ds.Spec.Template.Spec.HostNetwork {
		t.Fatal("hostNetwork must be false")
	}
	if ds.Spec.Template.Spec.DNSPolicy != corev1.DNSClusterFirstWithHostNet {
		t.Fatalf("dnsPolicy: got %v", ds.Spec.Template.Spec.DNSPolicy)
	}
	if len(ds.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("expected 1 container, got %d", len(ds.Spec.Template.Spec.Containers))
	}
	c := ds.Spec.Template.Spec.Containers[0]
	if c.Image != "ghcr.io/mwognicki/bulb:test" {
		t.Fatalf("image: got %q", c.Image)
	}
	wantArgs := []string{"proxy", "--drain-timeout=30s", "--upstream=0.0.0.0:8443=10.96.1.5:8443"}
	if !slices.Equal(c.Args, wantArgs) {
		t.Fatalf("args: got %v want %v", c.Args, wantArgs)
	}
	if len(c.Ports) != 1 || c.Ports[0].HostPort != 8443 || c.Ports[0].ContainerPort != 8443 {
		t.Fatalf("ports: got %+v", c.Ports)
	}
}

func TestBuildDaemonSet_DropsAllCapsAndHardens(t *testing.T) {
	ds, err := BuildDaemonSet(mkSvc(), "img", "bulb-system")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sc := ds.Spec.Template.Spec.Containers[0].SecurityContext
	if sc == nil {
		t.Fatal("nil security context")
	}
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Error("allowPrivilegeEscalation must be false")
	}
	if sc.Privileged == nil || *sc.Privileged {
		t.Error("privileged must be false")
	}
	if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		t.Error("runAsNonRoot must be true")
	}
	if sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
		t.Error("readOnlyRootFilesystem must be true")
	}
	if sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" {
		t.Errorf("expected Drop:[ALL], got %+v", sc.Capabilities)
	}
}

func TestBuildDaemonSet_PrivilegedPort(t *testing.T) {
	low := mkSvc(func(s *corev1.Service) {
		s.Spec.Ports = []corev1.ServicePort{{Name: "http", Port: 80, TargetPort: intstr.FromInt32(80)}}
	})

	if _, err := BuildDaemonSet(low, "img", "bulb-system"); !errors.Is(err, ErrPrivilegedPortDenied) {
		t.Fatalf("expected ErrPrivilegedPortDenied for port 80, got %v", err)
	}

	allowed := mkSvc(func(s *corev1.Service) {
		s.Annotations = map[string]string{AnnotationAllowPrivilegedPort: "true"}
		s.Spec.Ports = []corev1.ServicePort{{Name: "http", Port: 80, TargetPort: intstr.FromInt32(80)}}
	})
	ds, err := BuildDaemonSet(allowed, "img", "bulb-system")
	if err != nil {
		t.Fatalf("expected success with opt-in annotation, got %v", err)
	}
	if ds.Spec.Template.Spec.Containers[0].Ports[0].HostPort != 80 {
		t.Fatalf("hostPort: got %d", ds.Spec.Template.Spec.Containers[0].Ports[0].HostPort)
	}
}

func TestBuildDaemonSet_NodeSelector(t *testing.T) {
	svc := mkSvc(func(s *corev1.Service) {
		s.Annotations = map[string]string{AnnotationNodes: "role=edge,zone=eu-central"}
	})
	ds, err := BuildDaemonSet(svc, "img", "bulb-system")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := ds.Spec.Template.Spec.NodeSelector
	if got["role"] != "edge" || got["zone"] != "eu-central" || len(got) != 2 {
		t.Fatalf("nodeSelector: got %+v", got)
	}
}

func TestBuildDaemonSet_RejectsNonTCP(t *testing.T) {
	svc := mkSvc(func(s *corev1.Service) {
		s.Spec.Ports[0].Protocol = corev1.ProtocolUDP
	})
	if _, err := BuildDaemonSet(svc, "img", "bulb-system"); err == nil {
		t.Fatal("expected error for UDP in Phase 1")
	}
}

func TestBuildDaemonSet_RejectsHeadlessOrPortless(t *testing.T) {
	headless := mkSvc(func(s *corev1.Service) { s.Spec.ClusterIP = corev1.ClusterIPNone })
	if _, err := BuildDaemonSet(headless, "img", "bulb-system"); err == nil {
		t.Fatal("expected error for headless service")
	}

	portless := mkSvc(func(s *corev1.Service) { s.Spec.Ports = nil })
	if _, err := BuildDaemonSet(portless, "img", "bulb-system"); err == nil {
		t.Fatal("expected error for service with no ports")
	}

	if _, err := BuildDaemonSet(nil, "img", "bulb-system"); err == nil {
		t.Fatal("expected error for nil service")
	}
}

func TestBuildDaemonSet_MultiPort(t *testing.T) {
	svc := mkSvc(func(s *corev1.Service) {
		s.Spec.Ports = []corev1.ServicePort{
			{Name: "https", Port: 8443, TargetPort: intstr.FromInt32(9443), Protocol: corev1.ProtocolTCP},
			{Name: "metrics", Port: 9100, TargetPort: intstr.FromInt32(9100), Protocol: corev1.ProtocolTCP},
		}
	})
	ds, err := BuildDaemonSet(svc, "img", "bulb-system")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	args := ds.Spec.Template.Spec.Containers[0].Args
	wantUpstreams := []string{
		"--upstream=0.0.0.0:8443=10.96.1.5:9443",
		"--upstream=0.0.0.0:9100=10.96.1.5:9100",
	}
	for _, w := range wantUpstreams {
		if !slices.Contains(args, w) {
			t.Errorf("missing arg %q in %v", w, args)
		}
	}
	ports := ds.Spec.Template.Spec.Containers[0].Ports
	if len(ports) != 2 {
		t.Fatalf("expected 2 container ports, got %d", len(ports))
	}
}

func TestDaemonSetName(t *testing.T) {
	got := DaemonSetName(mkSvc())
	if got != "bulb-demo-echo" {
		t.Fatalf("got %q", got)
	}
}
