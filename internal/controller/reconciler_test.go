package controller

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
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
		WithStatusSubresource(&corev1.Service{}).
		Build()
	return &ServiceReconciler{
		Client:           c,
		Scheme:           scheme,
		Namespace:        "bulb-system",
		Image:            "ghcr.io/mwognicki/bulb:test",
		NodeIPsConfigMap: "node-ips",
	}, c
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

func TestReconcile_PopulatesStatusIngressFromConfigMap(t *testing.T) {
	svc := newSvc(nil)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "bulb-system", Name: "node-ips"},
		Data: map[string]string{
			"node-a": "203.0.113.10",
			"node-b": "203.0.113.11",
			"node-c": "203.0.113.12",
		},
	}
	r, c := newReconciler(t, svc, cm)

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

func TestReconcile_NoConfigMapMeansNoIngress(t *testing.T) {
	svc := newSvc(nil)
	r, c := newReconciler(t, svc)

	reconcileOnce(t, r, svc.Namespace, svc.Name)

	var got corev1.Service
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: svc.Namespace, Name: svc.Name}, &got); err != nil {
		t.Fatalf("get service: %v", err)
	}
	if len(got.Status.LoadBalancer.Ingress) != 0 {
		t.Fatalf("expected empty ingress without ConfigMap, got %+v", got.Status.LoadBalancer.Ingress)
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
