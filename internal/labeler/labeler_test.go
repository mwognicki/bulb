package labeler

import (
	"context"
	"net"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func fakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	if err := scheme.AddToScheme(s); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
}

func TestAnnotateNode_SetsIPv4(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "test-node"}}
	c := fakeClient(t, node)

	err := annotateNode(context.Background(), c, "test-node", "1.2.3.4", "")
	if err != nil {
		t.Fatalf("annotateNode: %v", err)
	}

	var got corev1.Node
	if err := c.Get(context.Background(), client.ObjectKey{Name: "test-node"}, &got); err != nil {
		t.Fatalf("get node: %v", err)
	}
	if got.Annotations[AnnotationIPv4] != "1.2.3.4" {
		t.Fatalf("ipv4 annotation: got %q", got.Annotations[AnnotationIPv4])
	}
}

func TestAnnotateNode_SetsIPv6(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "test-node"}}
	c := fakeClient(t, node)

	err := annotateNode(context.Background(), c, "test-node", "", "2001:db8::1")
	if err != nil {
		t.Fatalf("annotateNode: %v", err)
	}

	var got corev1.Node
	if err := c.Get(context.Background(), client.ObjectKey{Name: "test-node"}, &got); err != nil {
		t.Fatalf("get node: %v", err)
	}
	if got.Annotations[AnnotationIPv6] != "2001:db8::1" {
		t.Fatalf("ipv6 annotation: got %q", got.Annotations[AnnotationIPv6])
	}
}

func TestAnnotateNode_SetsBoth(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "test-node"}}
	c := fakeClient(t, node)

	err := annotateNode(context.Background(), c, "test-node", "1.2.3.4", "2001:db8::1")
	if err != nil {
		t.Fatalf("annotateNode: %v", err)
	}

	var got corev1.Node
	if err := c.Get(context.Background(), client.ObjectKey{Name: "test-node"}, &got); err != nil {
		t.Fatalf("get node: %v", err)
	}
	if got.Annotations[AnnotationIPv4] != "1.2.3.4" {
		t.Fatalf("ipv4: got %q", got.Annotations[AnnotationIPv4])
	}
	if got.Annotations[AnnotationIPv6] != "2001:db8::1" {
		t.Fatalf("ipv6: got %q", got.Annotations[AnnotationIPv6])
	}
}

func TestAnnotateNode_NoOpWhenUnchanged(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name: "test-node",
		Annotations: map[string]string{
			AnnotationIPv4: "1.2.3.4",
		},
	}}
	c := fakeClient(t, node)

	err := annotateNode(context.Background(), c, "test-node", "1.2.3.4", "")
	if err != nil {
		t.Fatalf("annotateNode: %v", err)
	}

	var got corev1.Node
	if err := c.Get(context.Background(), client.ObjectKey{Name: "test-node"}, &got); err != nil {
		t.Fatalf("get node: %v", err)
	}
	if got.ResourceVersion != node.ResourceVersion {
		t.Fatal("expected no update when annotations unchanged")
	}
}

func TestAnnotateNode_UpdatesExisting(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name: "test-node",
		Annotations: map[string]string{
			AnnotationIPv4: "1.2.3.4",
		},
	}}
	c := fakeClient(t, node)

	err := annotateNode(context.Background(), c, "test-node", "5.6.7.8", "")
	if err != nil {
		t.Fatalf("annotateNode: %v", err)
	}

	var got corev1.Node
	if err := c.Get(context.Background(), client.ObjectKey{Name: "test-node"}, &got); err != nil {
		t.Fatalf("get node: %v", err)
	}
	if got.Annotations[AnnotationIPv4] != "5.6.7.8" {
		t.Fatalf("ipv4: got %q want 5.6.7.8", got.Annotations[AnnotationIPv4])
	}
}

func TestReconcileOnce_UsesDetectFunction(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "test-node"}}
	c := fakeClient(t, node)

	detect := func() (string, string, error) {
		return "10.0.0.1", "fe80::1", nil
	}

	err := ReconcileOnce(context.Background(), c, "test-node", detect)
	if err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}

	var got corev1.Node
	if err := c.Get(context.Background(), client.ObjectKey{Name: "test-node"}, &got); err != nil {
		t.Fatalf("get node: %v", err)
	}
	if got.Annotations[AnnotationIPv4] != "10.0.0.1" {
		t.Fatalf("ipv4: got %q", got.Annotations[AnnotationIPv4])
	}
	if got.Annotations[AnnotationIPv6] != "fe80::1" {
		t.Fatalf("ipv6: got %q", got.Annotations[AnnotationIPv6])
	}
}

func TestIsPrivateV4(t *testing.T) {
	tests := []struct {
		ip   string
		want bool
	}{
		{"10.0.0.1", true},
		{"172.16.0.1", true},
		{"192.168.1.1", true},
		{"100.64.0.1", true},
		{"203.0.113.10", false},
		{"1.2.3.4", false},
		{"8.8.8.8", false},
	}
	for _, tc := range tests {
		t.Run(tc.ip, func(t *testing.T) {
			got := isPrivateV4(parseIP(tc.ip))
			if got != tc.want {
				t.Fatalf("isPrivateV4(%s) = %v want %v", tc.ip, got, tc.want)
			}
		})
	}
}

func parseIP(s string) net.IP {
	ip := net.ParseIP(s)
	if ip == nil {
		panic("invalid IP: " + s)
	}
	return ip
}
