package firewall

import (
	"context"
	"testing"

	bulbv1alpha1 "github.com/mwognicki/bulb/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("add kube scheme: %v", err)
	}
	if err := bulbv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add bulb scheme: %v", err)
	}
	return s
}

func TestDesiredPortsForNode_FiltersAndDedupes(t *testing.T) {
	lbports := []bulbv1alpha1.LBPort{
		{Spec: bulbv1alpha1.LBPortSpec{Port: 8080, Protocol: corev1.ProtocolTCP, Nodes: []string{"*"}}},
		{Spec: bulbv1alpha1.LBPortSpec{Port: 8080, Protocol: corev1.ProtocolTCP, Nodes: []string{"node-a"}}},
		{Spec: bulbv1alpha1.LBPortSpec{Port: 9090, Protocol: corev1.ProtocolTCP, Nodes: []string{"node-b"}}},
		{Spec: bulbv1alpha1.LBPortSpec{Port: 5353, Protocol: corev1.ProtocolUDP, Nodes: []string{"node-a"}}},
	}

	got := DesiredPortsForNode(lbports, "node-a")
	want := []PortSpec{
		{Port: 5353, Protocol: corev1.ProtocolUDP},
		{Port: 8080, Protocol: corev1.ProtocolTCP},
	}
	if len(got) != len(want) {
		t.Fatalf("port count: got %d want %d (%+v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ports[%d]: got %+v want %+v", i, got[i], want[i])
		}
	}
}

func TestDesiredPortsForNode_DefaultsEmptyProtocolToTCP(t *testing.T) {
	lbports := []bulbv1alpha1.LBPort{
		{Spec: bulbv1alpha1.LBPortSpec{Port: 8080, Nodes: []string{"*"}}},
	}

	got := DesiredPortsForNode(lbports, "node-a")
	if len(got) != 1 || got[0].Protocol != corev1.ProtocolTCP {
		t.Fatalf("expected default TCP, got %+v", got)
	}
}

func TestReconcile_ComputesAndStoresDesiredPorts(t *testing.T) {
	scheme := newScheme(t)
	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(
			&bulbv1alpha1.LBPort{
				ObjectMeta: metav1.ObjectMeta{Name: "bulb-demo-echo-8080-tcp"},
				Spec: bulbv1alpha1.LBPortSpec{
					Port:     8080,
					Protocol: corev1.ProtocolTCP,
					Nodes:    []string{"*"},
					Owner:    "bulb-controller",
				},
			},
			&bulbv1alpha1.LBPort{
				ObjectMeta: metav1.ObjectMeta{Name: "bulb-demo-dns-5353-udp"},
				Spec: bulbv1alpha1.LBPortSpec{
					Port:     5353,
					Protocol: corev1.ProtocolUDP,
					Nodes:    []string{"node-a"},
					Owner:    "bulb-controller",
				},
			},
			&bulbv1alpha1.LBPort{
				ObjectMeta: metav1.ObjectMeta{Name: "bulb-demo-admin-8443-tcp"},
				Spec: bulbv1alpha1.LBPortSpec{
					Port:     8443,
					Protocol: corev1.ProtocolTCP,
					Nodes:    []string{"node-b"},
					Owner:    "bulb-controller",
				},
			},
		).
		Build()

	r := &AgentReconciler{Client: client, NodeName: "node-a"}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := r.LastDesired()
	want := []PortSpec{
		{Port: 5353, Protocol: corev1.ProtocolUDP},
		{Port: 8080, Protocol: corev1.ProtocolTCP},
	}
	if len(got) != len(want) {
		t.Fatalf("port count: got %d want %d (%+v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ports[%d]: got %+v want %+v", i, got[i], want[i])
		}
	}
}
