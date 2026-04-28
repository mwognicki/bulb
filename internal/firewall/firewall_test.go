package firewall

import (
	"context"
	"errors"
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
		{Spec: bulbv1alpha1.LBPortSpec{Port: 5353, Protocol: corev1.ProtocolUDP, Nodes: []string{"node-a"}, AllowPrivileged: true}},
	}

	got := DesiredPortsForNode(lbports, "node-a")
	want := []PortSpec{
		{Port: 5353, Protocol: corev1.ProtocolUDP, AllowPrivileged: true},
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

func TestFirewallPolicy_Filter(t *testing.T) {
	policy := FirewallPolicy{DeniedPorts: []int32{22, 80, 443}}

	got := policy.Filter([]PortSpec{
		{Port: 22, Protocol: corev1.ProtocolTCP, AllowPrivileged: true},
		{Port: 80, Protocol: corev1.ProtocolTCP, AllowPrivileged: true},
		{Port: 81, Protocol: corev1.ProtocolTCP},
		{Port: 8443, Protocol: corev1.ProtocolTCP},
		{Port: 5353, Protocol: corev1.ProtocolUDP},
	})

	want := []PortSpec{
		{Port: 8443, Protocol: corev1.ProtocolTCP},
		{Port: 5353, Protocol: corev1.ProtocolUDP},
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

	r := &AgentReconciler{
		Client:   client,
		NodeName: "node-a",
		Backend:  &fakeBackend{},
		Policy:   FirewallPolicy{DeniedPorts: []int32{22, 80, 443}},
	}
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

func TestReconcile_AppliesBackendWithPolicyFilteredPorts(t *testing.T) {
	scheme := newScheme(t)
	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(
			&bulbv1alpha1.LBPort{
				ObjectMeta: metav1.ObjectMeta{Name: "bulb-demo-http-80-tcp"},
				Spec: bulbv1alpha1.LBPortSpec{
					Port:            80,
					Protocol:        corev1.ProtocolTCP,
					Nodes:           []string{"node-a"},
					AllowPrivileged: true,
				},
			},
			&bulbv1alpha1.LBPort{
				ObjectMeta: metav1.ObjectMeta{Name: "bulb-demo-admin-8443-tcp"},
				Spec: bulbv1alpha1.LBPortSpec{
					Port:     8443,
					Protocol: corev1.ProtocolTCP,
					Nodes:    []string{"node-a"},
				},
			},
		).
		Build()

	backend := &fakeBackend{}
	r := &AgentReconciler{
		Client:   client,
		NodeName: "node-a",
		Backend:  backend,
		Policy:   FirewallPolicy{DeniedPorts: []int32{22, 80, 443}},
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	want := []PortSpec{{Port: 8443, Protocol: corev1.ProtocolTCP}}
	if len(backend.applied) != len(want) {
		t.Fatalf("applied count: got %d want %d (%+v)", len(backend.applied), len(want), backend.applied)
	}
	for i := range want {
		if backend.applied[i] != want[i] {
			t.Fatalf("applied[%d]: got %+v want %+v", i, backend.applied[i], want[i])
		}
	}
}

type fakeBackend struct {
	applied []PortSpec
	err     error
}

func (b *fakeBackend) Name() string { return "fake" }

func (b *fakeBackend) Apply(_ context.Context, desired []PortSpec) error {
	if b.err != nil {
		return b.err
	}
	b.applied = append([]PortSpec(nil), desired...)
	return nil
}

func TestAgentReconciler_RequiresBackend(t *testing.T) {
	scheme := newScheme(t)
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &AgentReconciler{Client: client, NodeName: "node-a"}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err == nil {
		t.Fatal("expected reconcile to fail without backend")
	}
}

func TestFakeBackend_Error(t *testing.T) {
	backend := &fakeBackend{err: errors.New("boom")}
	if err := backend.Apply(context.Background(), nil); err == nil {
		t.Fatal("expected backend error")
	}
}
