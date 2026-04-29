package controller

import (
	"context"
	"slices"
	"strings"
	"testing"

	bulbv1alpha1 "github.com/mwognicki/bulb/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	if err := bulbv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add bulb scheme: %v", err)
	}
	return s
}

func newSvc(class *string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "demo",
			Name:      "echo",
			UID:       "svc-uid-123",
		},
		Spec: corev1.ServiceSpec{
			Type:              corev1.ServiceTypeLoadBalancer,
			ClusterIP:         "10.96.1.5",
			LoadBalancerClass: class,
			Ports: []corev1.ServicePort{
				{Name: "https", Port: 8443, TargetPort: intstr.FromInt32(8443), Protocol: corev1.ProtocolTCP},
			},
		},
	}
}

func newReconciler(t *testing.T, objs ...client.Object) (*ServiceReconciler, client.Client) {
	t.Helper()
	scheme := newScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&corev1.Service{}, &bulbv1alpha1.LBPort{}, &bulbv1alpha1.DNSRecord{}).
		Build()
	return &ServiceReconciler{
		Client:    c,
		Scheme:    scheme,
		Namespace: "bulb-system",
		Image:     "ghcr.io/mwognicki/bulb:test",
	}, c
}

func newReconcilerWithRecorder(t *testing.T, objs ...client.Object) (*ServiceReconciler, *record.FakeRecorder, client.Client) {
	t.Helper()
	scheme := newScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&corev1.Service{}, &bulbv1alpha1.LBPort{}, &bulbv1alpha1.DNSRecord{}).
		Build()
	rec := record.NewFakeRecorder(16)
	return &ServiceReconciler{
		Client:        c,
		Scheme:        scheme,
		EventRecorder: rec,
		Namespace:     "bulb-system",
		Image:         "ghcr.io/mwognicki/bulb:test",
	}, rec, c
}

func reconcileOnce(t *testing.T, r *ServiceReconciler, ns, name string) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: ns, Name: name},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

func TestReconcile_CreatesDaemonSetForBulbClass(t *testing.T) {
	cls := "bulb"
	svc := newSvc(&cls)
	r, c := newReconciler(t, svc)

	reconcileOnce(t, r, svc.Namespace, svc.Name)

	var ds appsv1.DaemonSet
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "bulb-system", Name: "bulb-demo-echo"}, &ds); err != nil {
		t.Fatalf("expected DaemonSet to exist: %v", err)
	}
	// Cross-namespace OwnerReferences are forbidden, so DS is tagged with
	// labels pointing back at the Service for cleanup correlation.
	if ds.Labels["bulb.toturi.tech/service"] != svc.Name || ds.Labels["bulb.toturi.tech/service-namespace"] != svc.Namespace {
		t.Fatalf("expected service-tracking labels, got %+v", ds.Labels)
	}
}

func TestReconcile_CreatesLBPortsForService(t *testing.T) {
	svc := newSvc(nil)
	r, c := newReconciler(t, svc)

	reconcileOnce(t, r, svc.Namespace, svc.Name)

	var lbport bulbv1alpha1.LBPort
	if err := c.Get(context.Background(), types.NamespacedName{Name: "bulb-demo-echo-8443-tcp"}, &lbport); err != nil {
		t.Fatalf("expected LBPort to exist: %v", err)
	}
	if lbport.Spec.Port != 8443 || lbport.Spec.Protocol != corev1.ProtocolTCP {
		t.Fatalf("unexpected lbport spec: %+v", lbport.Spec)
	}
	if !sameStringSet(lbport.Spec.Nodes, []string{"*"}) {
		t.Fatalf("expected wildcard nodes, got %+v", lbport.Spec.Nodes)
	}
}

func TestReconcile_CreatesNodeScopedLBPortsWhenSelectorPresent(t *testing.T) {
	svc := newSvc(nil)
	svc.Annotations = map[string]string{AnnotationNodes: "role=edge"}
	nodes := []client.Object{
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "node-a",
				Labels: map[string]string{"role": "edge"},
			},
		},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "node-b",
				Labels: map[string]string{"role": "edge"},
			},
		},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "node-c",
				Labels: map[string]string{"role": "db"},
			},
		},
	}
	objects := append([]client.Object{svc}, nodes...)
	r, c := newReconciler(t, objects...)

	reconcileOnce(t, r, svc.Namespace, svc.Name)

	var lbport bulbv1alpha1.LBPort
	if err := c.Get(context.Background(), types.NamespacedName{Name: "bulb-demo-echo-8443-tcp"}, &lbport); err != nil {
		t.Fatalf("expected LBPort to exist: %v", err)
	}
	if !sameStringSet(lbport.Spec.Nodes, []string{"node-a", "node-b"}) {
		t.Fatalf("expected selected nodes, got %+v", lbport.Spec.Nodes)
	}
}

func TestReconcile_CleansUpOnServiceDelete(t *testing.T) {
	svc := newSvc(nil)
	r, c := newReconciler(t, svc)

	reconcileOnce(t, r, svc.Namespace, svc.Name)
	if err := c.Delete(context.Background(), svc); err != nil {
		t.Fatalf("delete service: %v", err)
	}
	reconcileOnce(t, r, svc.Namespace, svc.Name)

	var ds appsv1.DaemonSet
	err := c.Get(context.Background(), types.NamespacedName{Namespace: "bulb-system", Name: "bulb-demo-echo"}, &ds)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected DS to be cleaned up after Service deletion, got err=%v", err)
	}

	var lbport bulbv1alpha1.LBPort
	err = c.Get(context.Background(), types.NamespacedName{Name: "bulb-demo-echo-8443-tcp"}, &lbport)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected LBPort to be cleaned up after Service deletion, got err=%v", err)
	}
}

func TestReconcile_CreatesDaemonSetForEmptyClass(t *testing.T) {
	svc := newSvc(nil)
	r, c := newReconciler(t, svc)

	reconcileOnce(t, r, svc.Namespace, svc.Name)

	var ds appsv1.DaemonSet
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "bulb-system", Name: "bulb-demo-echo"}, &ds); err != nil {
		t.Fatalf("expected DaemonSet for empty loadBalancerClass: %v", err)
	}
}

func TestReconcile_LocalExternalTrafficPolicyPassesReadyEndpoints(t *testing.T) {
	svc := newSvc(nil)
	svc.Annotations = map[string]string{AnnotationExternalTrafficPolicy: "Local"}
	eps := &corev1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{Namespace: svc.Namespace, Name: svc.Name},
		Subsets: []corev1.EndpointSubset{{
			Addresses:         []corev1.EndpointAddress{{IP: "10.244.1.8"}, {IP: "10.244.1.7"}},
			NotReadyAddresses: []corev1.EndpointAddress{{IP: "10.244.1.9"}},
			Ports:             []corev1.EndpointPort{{Name: "https", Port: 9443, Protocol: corev1.ProtocolTCP}},
		}},
	}
	r, c := newReconciler(t, svc, eps)

	reconcileOnce(t, r, svc.Namespace, svc.Name)

	var ds appsv1.DaemonSet
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "bulb-system", Name: "bulb-demo-echo"}, &ds); err != nil {
		t.Fatalf("expected DaemonSet to exist: %v", err)
	}
	args := ds.Spec.Template.Spec.Containers[0].Args
	want := "--endpoint=0.0.0.0:8443=10.244.1.7:9443,10.244.1.8:9443"
	if !slices.Contains(args, want) {
		t.Fatalf("missing endpoint arg %q in %v", want, args)
	}
}

func TestReconcile_IgnoresOtherClass(t *testing.T) {
	cls := "metallb"
	svc := newSvc(&cls)
	r, c := newReconciler(t, svc)

	reconcileOnce(t, r, svc.Namespace, svc.Name)

	var ds appsv1.DaemonSet
	err := c.Get(context.Background(), types.NamespacedName{Namespace: "bulb-system", Name: "bulb-demo-echo"}, &ds)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected NotFound for non-bulb class, got %v", err)
	}
}

func TestReconcile_IgnoresClusterIPType(t *testing.T) {
	svc := newSvc(nil)
	svc.Spec.Type = corev1.ServiceTypeClusterIP

	r, c := newReconciler(t, svc)
	reconcileOnce(t, r, svc.Namespace, svc.Name)

	var ds appsv1.DaemonSet
	err := c.Get(context.Background(), types.NamespacedName{Namespace: "bulb-system", Name: "bulb-demo-echo"}, &ds)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected NotFound for ClusterIP service, got %v", err)
	}
}

func TestReconcile_PopulatesStatusIngressFromNodeAnnotations(t *testing.T) {
	svc := newSvc(nil)
	nodes := []client.Object{
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{
			Name:        "node-a",
			Annotations: map[string]string{"bulb.toturi.tech/public-ipv4": "203.0.113.10"},
		}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{
			Name:        "node-b",
			Annotations: map[string]string{"bulb.toturi.tech/public-ipv4": "203.0.113.11"},
		}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{
			Name:        "node-c",
			Annotations: map[string]string{"bulb.toturi.tech/public-ipv4": "203.0.113.12"},
		}},
	}
	r, c := newReconciler(t, append([]client.Object{svc}, nodes...)...)

	reconcileOnce(t, r, svc.Namespace, svc.Name)

	var got corev1.Service
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: svc.Namespace, Name: svc.Name}, &got); err != nil {
		t.Fatalf("get service: %v", err)
	}
	want := []string{"203.0.113.10", "203.0.113.11", "203.0.113.12"}
	if len(got.Status.LoadBalancer.Ingress) != len(want) {
		t.Fatalf("ingress count: got %d want %d (%+v)", len(got.Status.LoadBalancer.Ingress), len(want), got.Status.LoadBalancer.Ingress)
	}
	for i, ip := range want {
		if got.Status.LoadBalancer.Ingress[i].IP != ip {
			t.Errorf("ingress[%d]: got %q want %q", i, got.Status.LoadBalancer.Ingress[i].IP, ip)
		}
	}
}

func TestReconcile_NoAnnotationsMeansNoIngress(t *testing.T) {
	svc := newSvc(nil)
	r, c := newReconciler(t, svc)

	reconcileOnce(t, r, svc.Namespace, svc.Name)

	var got corev1.Service
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: svc.Namespace, Name: svc.Name}, &got); err != nil {
		t.Fatalf("get service: %v", err)
	}
	if len(got.Status.LoadBalancer.Ingress) != 0 {
		t.Fatalf("expected empty ingress without node annotations, got %+v", got.Status.LoadBalancer.Ingress)
	}
}

func TestReconcile_SkipsUnschedulableNodesForIPs(t *testing.T) {
	svc := newSvc(nil)
	nodes := []client.Object{
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{
			Name:        "node-a",
			Annotations: map[string]string{"bulb.toturi.tech/public-ipv4": "203.0.113.10"},
		}},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "node-b",
				Annotations: map[string]string{"bulb.toturi.tech/public-ipv4": "203.0.113.11"},
			},
			Spec: corev1.NodeSpec{Unschedulable: true},
		},
	}
	r, c := newReconciler(t, append([]client.Object{svc}, nodes...)...)

	reconcileOnce(t, r, svc.Namespace, svc.Name)

	var got corev1.Service
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: svc.Namespace, Name: svc.Name}, &got); err != nil {
		t.Fatalf("get service: %v", err)
	}
	if len(got.Status.LoadBalancer.Ingress) != 1 || got.Status.LoadBalancer.Ingress[0].IP != "203.0.113.10" {
		t.Fatalf("expected only schedulable node IP, got %+v", got.Status.LoadBalancer.Ingress)
	}
}

func TestReconcile_CleansUpOnTypeChange(t *testing.T) {
	svc := newSvc(nil)
	r, c := newReconciler(t, svc)

	reconcileOnce(t, r, svc.Namespace, svc.Name)

	// Flip the Service to ClusterIP — reconciler should remove the DS.
	var live corev1.Service
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: svc.Namespace, Name: svc.Name}, &live); err != nil {
		t.Fatalf("get service: %v", err)
	}
	live.Spec.Type = corev1.ServiceTypeClusterIP
	if err := c.Update(context.Background(), &live); err != nil {
		t.Fatalf("update: %v", err)
	}

	reconcileOnce(t, r, svc.Namespace, svc.Name)

	var ds appsv1.DaemonSet
	err := c.Get(context.Background(), types.NamespacedName{Namespace: "bulb-system", Name: "bulb-demo-echo"}, &ds)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected DaemonSet to be cleaned up, got err=%v", err)
	}

	var lbport bulbv1alpha1.LBPort
	err = c.Get(context.Background(), types.NamespacedName{Name: "bulb-demo-echo-8443-tcp"}, &lbport)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected LBPort to be cleaned up, got err=%v", err)
	}
}

func TestServiceMatches(t *testing.T) {
	bulb := "bulb"
	other := "metallb"
	tests := []struct {
		name string
		svc  *corev1.Service
		want bool
	}{
		{"loadbalancer no class", &corev1.Service{Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer}}, true},
		{"loadbalancer bulb class", &corev1.Service{Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer, LoadBalancerClass: &bulb}}, true},
		{"loadbalancer other class", &corev1.Service{Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer, LoadBalancerClass: &other}}, false},
		{"clusterip", &corev1.Service{Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP}}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ServiceMatches(tc.svc); got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func drainEvents(rec *record.FakeRecorder) []string {
	var events []string
	for {
		select {
		case e, ok := <-rec.Events:
			if !ok {
				return events
			}
			events = append(events, e)
		default:
			return events
		}
	}
}

func hasEventWith(events []string, substr string) bool {
	for _, e := range events {
		if strings.Contains(e, substr) {
			return true
		}
	}
	return false
}

func TestEmitLBPortEvents_PortOpened(t *testing.T) {
	svc := newSvc(nil)
	nodes := []client.Object{
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-b"}},
	}
	r, rec, _ := newReconcilerWithRecorder(t, append([]client.Object{svc}, nodes...)...)
	reconcileOnce(t, r, svc.Namespace, svc.Name)

	var lbport bulbv1alpha1.LBPort
	if err := r.Get(context.Background(), types.NamespacedName{Name: "bulb-demo-echo-8443-tcp"}, &lbport); err != nil {
		t.Fatalf("get lbport: %v", err)
	}
	lbport.Status.AppliedNodes = []string{"node-a", "node-b"}
	if err := r.Status().Update(context.Background(), &lbport); err != nil {
		t.Fatalf("update status: %v", err)
	}

	reconcileOnce(t, r, svc.Namespace, svc.Name)

	events := drainEvents(rec)
	if !hasEventWith(events, "FirewallPortOpened") {
		t.Fatalf("expected FirewallPortOpened event, got %v", events)
	}
}

func TestEmitLBPortEvents_PartiallyApplied(t *testing.T) {
	svc := newSvc(nil)
	nodes := []client.Object{
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-b"}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-c"}},
	}
	r, rec, _ := newReconcilerWithRecorder(t, append([]client.Object{svc}, nodes...)...)
	reconcileOnce(t, r, svc.Namespace, svc.Name)

	var lbport bulbv1alpha1.LBPort
	if err := r.Get(context.Background(), types.NamespacedName{Name: "bulb-demo-echo-8443-tcp"}, &lbport); err != nil {
		t.Fatalf("get lbport: %v", err)
	}
	lbport.Status.AppliedNodes = []string{"node-a"}
	if err := r.Status().Update(context.Background(), &lbport); err != nil {
		t.Fatalf("update status: %v", err)
	}

	reconcileOnce(t, r, svc.Namespace, svc.Name)

	events := drainEvents(rec)
	if !hasEventWith(events, "FirewallPortPartiallyApplied") {
		t.Fatalf("expected FirewallPortPartiallyApplied event, got %v", events)
	}
}

func TestEmitLBPortEvents_PortClosed(t *testing.T) {
	svc := newSvc(nil)
	svc.Spec.Ports = []corev1.ServicePort{
		{Name: "https", Port: 8443, TargetPort: intstr.FromInt32(8443), Protocol: corev1.ProtocolTCP},
		{Name: "http", Port: 8080, TargetPort: intstr.FromInt32(8080), Protocol: corev1.ProtocolTCP},
	}
	r, rec, _ := newReconcilerWithRecorder(t, svc)
	reconcileOnce(t, r, svc.Namespace, svc.Name)

	var list bulbv1alpha1.LBPortList
	if err := r.List(context.Background(), &list); err != nil {
		t.Fatalf("list lbports: %v", err)
	}
	if len(list.Items) != 2 {
		t.Fatalf("expected 2 lbports, got %d", len(list.Items))
	}

	var live corev1.Service
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: svc.Namespace, Name: svc.Name}, &live); err != nil {
		t.Fatalf("get service: %v", err)
	}
	live.Spec.Ports = live.Spec.Ports[:1]
	if err := r.Update(context.Background(), &live); err != nil {
		t.Fatalf("update service: %v", err)
	}

	reconcileOnce(t, r, svc.Namespace, svc.Name)

	events := drainEvents(rec)
	if !hasEventWith(events, "FirewallPortClosed") {
		t.Fatalf("expected FirewallPortClosed event, got %v", events)
	}
}
