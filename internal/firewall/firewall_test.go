package firewall

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"testing"

	"github.com/go-logr/logr"
	bulbv1alpha1 "github.com/mwognicki/bulb/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
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

	got, rejected := policy.Filter([]PortSpec{
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
	if len(rejected) != 3 {
		t.Fatalf("rejected count: got %d want 3 (%+v)", len(rejected), rejected)
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

func (b *fakeBackend) Validate(context.Context) error { return b.err }

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

func TestAgentReconciler_ReadyzReflectsBackendValidation(t *testing.T) {
	r := &AgentReconciler{
		NodeName: "node-a",
		Backend:  &fakeBackend{},
	}
	if err := r.readyz(&http.Request{}); err != nil {
		t.Fatalf("expected readyz success, got %v", err)
	}

	r.Backend = &fakeBackend{err: errors.New("backend down")}
	if err := r.readyz(&http.Request{}); err == nil {
		t.Fatal("expected readyz failure")
	}
}

func buildFakeClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithStatusSubresource(&bulbv1alpha1.LBPort{}).
		WithObjects(objects...).
		Build()
}

func getAppliedNodes(t *testing.T, c client.Client, name string) []string {
	t.Helper()
	var lbport bulbv1alpha1.LBPort
	if err := c.Get(context.Background(), types.NamespacedName{Name: name}, &lbport); err != nil {
		t.Fatalf("get lbport %s: %v", name, err)
	}
	return lbport.Status.AppliedNodes
}

func TestEnsureNodeInStatus_AddsNode(t *testing.T) {
	lbport := &bulbv1alpha1.LBPort{
		ObjectMeta: metav1.ObjectMeta{Name: "test-8080-tcp"},
		Spec:       bulbv1alpha1.LBPortSpec{Port: 8080, Protocol: corev1.ProtocolTCP},
	}
	c := buildFakeClient(t, lbport)
	r := &AgentReconciler{Client: c, NodeName: "node-a", Backend: &fakeBackend{}}

	r.ensureNodeInStatus(context.Background(), logr.Discard(), lbport)

	got := getAppliedNodes(t, c, "test-8080-tcp")
	if !slices.Equal(got, []string{"node-a"}) {
		t.Fatalf("appliedNodes: got %v, want [node-a]", got)
	}
}

func TestEnsureNodeInStatus_Idempotent(t *testing.T) {
	lbport := &bulbv1alpha1.LBPort{
		ObjectMeta: metav1.ObjectMeta{Name: "test-8080-tcp"},
		Spec:       bulbv1alpha1.LBPortSpec{Port: 8080, Protocol: corev1.ProtocolTCP},
		Status:     bulbv1alpha1.LBPortStatus{AppliedNodes: []string{"node-a"}},
	}
	c := buildFakeClient(t, lbport)
	r := &AgentReconciler{Client: c, NodeName: "node-a", Backend: &fakeBackend{}}

	r.ensureNodeInStatus(context.Background(), logr.Discard(), lbport)

	got := getAppliedNodes(t, c, "test-8080-tcp")
	if !slices.Equal(got, []string{"node-a"}) {
		t.Fatalf("appliedNodes: got %v, want [node-a] (no duplicate)", got)
	}
}

func TestEnsureNodeInStatus_AppendsSorted(t *testing.T) {
	lbport := &bulbv1alpha1.LBPort{
		ObjectMeta: metav1.ObjectMeta{Name: "test-8080-tcp"},
		Spec:       bulbv1alpha1.LBPortSpec{Port: 8080, Protocol: corev1.ProtocolTCP},
		Status:     bulbv1alpha1.LBPortStatus{AppliedNodes: []string{"node-c", "node-b"}},
	}
	c := buildFakeClient(t, lbport)
	r := &AgentReconciler{Client: c, NodeName: "node-a", Backend: &fakeBackend{}}

	r.ensureNodeInStatus(context.Background(), logr.Discard(), lbport)

	got := getAppliedNodes(t, c, "test-8080-tcp")
	want := []string{"node-a", "node-b", "node-c"}
	if !slices.Equal(got, want) {
		t.Fatalf("appliedNodes: got %v, want %v", got, want)
	}
}

func TestEnsureNodeNotInStatus_RemovesNode(t *testing.T) {
	lbport := &bulbv1alpha1.LBPort{
		ObjectMeta: metav1.ObjectMeta{Name: "test-8080-tcp"},
		Spec:       bulbv1alpha1.LBPortSpec{Port: 8080, Protocol: corev1.ProtocolTCP},
		Status:     bulbv1alpha1.LBPortStatus{AppliedNodes: []string{"node-a", "node-b"}},
	}
	c := buildFakeClient(t, lbport)
	r := &AgentReconciler{Client: c, NodeName: "node-a", Backend: &fakeBackend{}}

	r.ensureNodeNotInStatus(context.Background(), logr.Discard(), lbport)

	got := getAppliedNodes(t, c, "test-8080-tcp")
	if !slices.Equal(got, []string{"node-b"}) {
		t.Fatalf("appliedNodes: got %v, want [node-b]", got)
	}
}

func TestEnsureNodeNotInStatus_Idempotent(t *testing.T) {
	lbport := &bulbv1alpha1.LBPort{
		ObjectMeta: metav1.ObjectMeta{Name: "test-8080-tcp"},
		Spec:       bulbv1alpha1.LBPortSpec{Port: 8080, Protocol: corev1.ProtocolTCP},
		Status:     bulbv1alpha1.LBPortStatus{AppliedNodes: []string{"node-b"}},
	}
	c := buildFakeClient(t, lbport)
	r := &AgentReconciler{Client: c, NodeName: "node-a", Backend: &fakeBackend{}}

	r.ensureNodeNotInStatus(context.Background(), logr.Discard(), lbport)

	got := getAppliedNodes(t, c, "test-8080-tcp")
	if !slices.Equal(got, []string{"node-b"}) {
		t.Fatalf("appliedNodes: got %v, want [node-b] (unchanged)", got)
	}
}

func TestEnsureNodeNotInStatus_EmptyListIsNil(t *testing.T) {
	lbport := &bulbv1alpha1.LBPort{
		ObjectMeta: metav1.ObjectMeta{Name: "test-8080-tcp"},
		Spec:       bulbv1alpha1.LBPortSpec{Port: 8080, Protocol: corev1.ProtocolTCP},
		Status:     bulbv1alpha1.LBPortStatus{AppliedNodes: []string{"node-a"}},
	}
	c := buildFakeClient(t, lbport)
	r := &AgentReconciler{Client: c, NodeName: "node-a", Backend: &fakeBackend{}}

	r.ensureNodeNotInStatus(context.Background(), logr.Discard(), lbport)

	got := getAppliedNodes(t, c, "test-8080-tcp")
	if len(got) != 0 {
		t.Fatalf("appliedNodes: got %v, want empty", got)
	}
}

func TestUpdateLBPortStatuses_AddsAppliedRemovesNotApplied(t *testing.T) {
	applied := &bulbv1alpha1.LBPort{
		ObjectMeta: metav1.ObjectMeta{Name: "applied-8080-tcp"},
		Spec:       bulbv1alpha1.LBPortSpec{Port: 8080, Protocol: corev1.ProtocolTCP, Nodes: []string{"node-a"}},
	}
	notApplied := &bulbv1alpha1.LBPort{
		ObjectMeta: metav1.ObjectMeta{Name: "not-applied-9090-tcp"},
		Spec:       bulbv1alpha1.LBPortSpec{Port: 9090, Protocol: corev1.ProtocolTCP, Nodes: []string{"node-b"}},
	}
	stale := &bulbv1alpha1.LBPort{
		ObjectMeta: metav1.ObjectMeta{Name: "stale-8443-tcp"},
		Spec:       bulbv1alpha1.LBPortSpec{Port: 8443, Protocol: corev1.ProtocolTCP, Nodes: []string{"node-a"}},
		Status:     bulbv1alpha1.LBPortStatus{AppliedNodes: []string{"node-a"}},
	}

	c := buildFakeClient(t, applied, notApplied, stale)
	r := &AgentReconciler{Client: c, NodeName: "node-a", Backend: &fakeBackend{}}

	r.updateLBPortStatuses(context.Background(), logr.Discard(),
		[]bulbv1alpha1.LBPort{*applied, *notApplied, *stale},
		[]PortSpec{{Port: 8080, Protocol: corev1.ProtocolTCP}},
	)

	if got := getAppliedNodes(t, c, "applied-8080-tcp"); !slices.Equal(got, []string{"node-a"}) {
		t.Fatalf("applied: got %v", got)
	}
	if got := getAppliedNodes(t, c, "not-applied-9090-tcp"); len(got) != 0 {
		t.Fatalf("not-applied: got %v, want empty", got)
	}
	if got := getAppliedNodes(t, c, "stale-8443-tcp"); len(got) != 0 {
		t.Fatalf("stale (port not in applied set): got %v, want empty", got)
	}
}

func TestReconcile_WritesLBPortStatus(t *testing.T) {
	scheme := newScheme(t)
	lbport := &bulbv1alpha1.LBPort{
		ObjectMeta: metav1.ObjectMeta{Name: "bulb-demo-echo-8080-tcp"},
		Spec: bulbv1alpha1.LBPortSpec{
			Port:     8080,
			Protocol: corev1.ProtocolTCP,
			Nodes:    []string{"node-a"},
			Owner:    "bulb-controller",
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&bulbv1alpha1.LBPort{}).
		WithObjects(lbport).
		Build()

	r := &AgentReconciler{
		Client:   c,
		NodeName: "node-a",
		Backend:  &fakeBackend{},
		Policy:   FirewallPolicy{},
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := getAppliedNodes(t, c, "bulb-demo-echo-8080-tcp")
	if !slices.Equal(got, []string{"node-a"}) {
		t.Fatalf("appliedNodes after reconcile: got %v, want [node-a]", got)
	}
}

func TestReconcile_DryRun_SkipsStatusUpdate(t *testing.T) {
	scheme := newScheme(t)
	lbport := &bulbv1alpha1.LBPort{
		ObjectMeta: metav1.ObjectMeta{Name: "bulb-demo-echo-8080-tcp"},
		Spec: bulbv1alpha1.LBPortSpec{
			Port:     8080,
			Protocol: corev1.ProtocolTCP,
			Nodes:    []string{"node-a"},
			Owner:    "bulb-controller",
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&bulbv1alpha1.LBPort{}).
		WithObjects(lbport).
		Build()

	r := &AgentReconciler{
		Client:   c,
		NodeName: "node-a",
		Backend:  &fakeBackend{},
		Policy:   FirewallPolicy{},
		DryRun:   true,
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := getAppliedNodes(t, c, "bulb-demo-echo-8080-tcp")
	if len(got) != 0 {
		t.Fatalf("dry-run should not write status, got %v", got)
	}
}

func TestReconcile_DeniedPortNotInStatus(t *testing.T) {
	scheme := newScheme(t)
	lbport := &bulbv1alpha1.LBPort{
		ObjectMeta: metav1.ObjectMeta{Name: "bulb-demo-http-80-tcp"},
		Spec: bulbv1alpha1.LBPortSpec{
			Port:     80,
			Protocol: corev1.ProtocolTCP,
			Nodes:    []string{"node-a"},
			Owner:    "bulb-controller",
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&bulbv1alpha1.LBPort{}).
		WithObjects(lbport).
		Build()

	r := &AgentReconciler{
		Client:   c,
		NodeName: "node-a",
		Backend:  &fakeBackend{},
		Policy:   FirewallPolicy{DeniedPorts: []int32{80}},
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := getAppliedNodes(t, c, "bulb-demo-http-80-tcp")
	if len(got) != 0 {
		t.Fatalf("denied port should not appear in status, got %v", got)
	}
}

func TestReconcile_PortRemovedFromSpec_ClearedFromStatus(t *testing.T) {
	scheme := newScheme(t)
	lbport := &bulbv1alpha1.LBPort{
		ObjectMeta: metav1.ObjectMeta{Name: "bulb-demo-echo-8080-tcp"},
		Spec: bulbv1alpha1.LBPortSpec{
			Port:     8080,
			Protocol: corev1.ProtocolTCP,
			Nodes:    []string{"node-b"},
			Owner:    "bulb-controller",
		},
		Status: bulbv1alpha1.LBPortStatus{AppliedNodes: []string{"node-a"}},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&bulbv1alpha1.LBPort{}).
		WithObjects(lbport).
		Build()

	r := &AgentReconciler{
		Client:   c,
		NodeName: "node-a",
		Backend:  &fakeBackend{},
		Policy:   FirewallPolicy{},
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := getAppliedNodes(t, c, "bulb-demo-echo-8080-tcp")
	if len(got) != 0 {
		t.Fatalf("node removed from spec.nodes should be cleared from status, got %v", got)
	}
}
