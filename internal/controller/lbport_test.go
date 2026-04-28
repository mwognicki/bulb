package controller

import (
	"context"
	"testing"

	bulbv1alpha1 "github.com/mwognicki/bulb/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestBuildLBPorts_DefaultsToAllNodes(t *testing.T) {
	svc := mkSvc()
	r, _ := newReconciler(t, svc)

	got, err := r.BuildLBPorts(context.Background(), svc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 LBPort, got %d", len(got))
	}
	if want := "bulb-demo-echo-8443-tcp"; got[0].Name != want {
		t.Fatalf("name: got %q want %q", got[0].Name, want)
	}
	if got[0].Spec.Owner != lbPortOwnerName {
		t.Fatalf("owner: got %q", got[0].Spec.Owner)
	}
	if !sameStringSet(got[0].Spec.Nodes, []string{"*"}) {
		t.Fatalf("nodes: got %+v", got[0].Spec.Nodes)
	}
}

func TestBuildLBPorts_ResolvesSelectedNodes(t *testing.T) {
	svc := mkSvc(func(s *corev1.Service) {
		s.Annotations = map[string]string{AnnotationNodes: "role in (edge,gateway)"}
	})
	r, _ := newReconciler(t,
		svc,
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "node-a", Labels: map[string]string{"role": "edge"}},
		},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "node-b", Labels: map[string]string{"role": "gateway"}},
		},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "node-c", Labels: map[string]string{"role": "db"}},
		},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "node-d", Labels: map[string]string{"role": "edge"}},
			Spec:       corev1.NodeSpec{Unschedulable: true},
		},
	)

	got, err := r.BuildLBPorts(context.Background(), svc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !sameStringSet(got[0].Spec.Nodes, []string{"node-a", "node-b"}) {
		t.Fatalf("nodes: got %+v", got[0].Spec.Nodes)
	}
}

func TestApplyLBPorts_RemovesStaleObjects(t *testing.T) {
	svc := mkSvc()
	stale := &bulbv1alpha1.LBPort{
		TypeMeta: metav1.TypeMeta{APIVersion: bulbv1alpha1.GroupVersion.String(), Kind: "LBPort"},
		ObjectMeta: metav1.ObjectMeta{
			Name:   "bulb-demo-echo-9000-tcp",
			Labels: serviceLabels(svc),
		},
		Spec: bulbv1alpha1.LBPortSpec{
			Port:     9000,
			Protocol: corev1.ProtocolTCP,
			Nodes:    []string{"*"},
			Owner:    lbPortOwnerName,
		},
	}
	r, c := newReconciler(t, svc, stale)

	desired, err := r.BuildLBPorts(context.Background(), svc)
	if err != nil {
		t.Fatalf("build lbports: %v", err)
	}
	if err := r.applyLBPorts(context.Background(), desired, svc); err != nil {
		t.Fatalf("apply lbports: %v", err)
	}

	var list bulbv1alpha1.LBPortList
	if err := c.List(context.Background(), &list); err != nil {
		t.Fatalf("list lbports: %v", err)
	}
	if got, want := names(list.Items), []string{"bulb-demo-echo-8443-tcp"}; !sameStringSet(got, want) {
		t.Fatalf("names: got %v want %v", got, want)
	}
}
