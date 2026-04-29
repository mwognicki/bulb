package controller

import (
	"context"
	"testing"

	bulbv1alpha1 "github.com/mwognicki/bulb/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func nodeWithIP(name, ipv4 string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:        name,
		Annotations: map[string]string{"bulb.toturi.tech/public-ipv4": ipv4},
	}}
}

func nodeWithIPs(name, ipv4, ipv6 string) *corev1.Node {
	ann := map[string]string{}
	if ipv4 != "" {
		ann["bulb.toturi.tech/public-ipv4"] = ipv4
	}
	if ipv6 != "" {
		ann["bulb.toturi.tech/public-ipv6"] = ipv6
	}
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: ann}}
}

func TestBuildDNSRecords_NilWhenNoAnnotation(t *testing.T) {
	svc := mkSvc()
	recs := BuildDNSRecords(svc, []string{"1.2.3.4"})
	if recs != nil {
		t.Fatalf("expected nil without dns-name annotation, got %+v", recs)
	}
}

func TestBuildDNSRecords_ProducesCorrectSpec(t *testing.T) {
	svc := mkSvc(func(s *corev1.Service) {
		s.Annotations = map[string]string{AnnotationDNSName: "api.example.com"}
	})
	recs := BuildDNSRecords(svc, []string{"203.0.113.12", "203.0.113.10", "203.0.113.11"})
	if len(recs) != 1 {
		t.Fatalf("expected 1 record (A only), got %d", len(recs))
	}
	rec := recs[0]
	if got, want := rec.Name, "bulb-demo-echo"; got != want {
		t.Fatalf("name: got %q want %q", got, want)
	}
	if got, want := rec.Spec.FQDN, "api.example.com"; got != want {
		t.Fatalf("fqdn: got %q want %q", got, want)
	}
	if got, want := rec.Spec.Type, "A"; got != want {
		t.Fatalf("type: got %q want %q", got, want)
	}
	if got, want := rec.Spec.TTL, int32(60); got != want {
		t.Fatalf("ttl: got %d want %d", got, want)
	}
	if !sameStringSet(rec.Spec.Targets, []string{"203.0.113.10", "203.0.113.11", "203.0.113.12"}) {
		t.Fatalf("targets should be sorted: got %v", rec.Spec.Targets)
	}
	l := rec.Labels
	if l[labelService] != svc.Name || l[labelServiceNs] != svc.Namespace || l[labelManagedBy] != labelManagedByV {
		t.Fatalf("labels: got %+v", l)
	}
}

func TestBuildDNSRecords_SortsTargets(t *testing.T) {
	svc := mkSvc(func(s *corev1.Service) {
		s.Annotations = map[string]string{AnnotationDNSName: "svc.example.com"}
	})
	recs := BuildDNSRecords(svc, []string{"10.0.0.3", "10.0.0.1", "10.0.0.2"})
	if len(recs) != 1 {
		t.Fatalf("expected 1 record, got %d", len(recs))
	}
	rec := recs[0]
	if rec.Spec.Targets[0] != "10.0.0.1" || rec.Spec.Targets[1] != "10.0.0.2" || rec.Spec.Targets[2] != "10.0.0.3" {
		t.Fatalf("targets not sorted: %v", rec.Spec.Targets)
	}
}

func TestBuildDNSRecords_EmptyTargetsKeepsADryRunSignal(t *testing.T) {
	svc := mkSvc(func(s *corev1.Service) {
		s.Annotations = map[string]string{AnnotationDNSName: "svc.example.com"}
	})
	recs := BuildDNSRecords(svc, nil)
	if len(recs) != 1 {
		t.Fatalf("expected single empty-A fallback, got %d", len(recs))
	}
	if recs[0].Spec.Type != "A" {
		t.Fatalf("expected fallback A record, got %q", recs[0].Spec.Type)
	}
	if len(recs[0].Spec.Targets) != 0 {
		t.Fatalf("expected empty targets, got %v", recs[0].Spec.Targets)
	}
}

func TestBuildDNSRecords_SplitsAandAAAA(t *testing.T) {
	svc := mkSvc(func(s *corev1.Service) {
		s.Annotations = map[string]string{AnnotationDNSName: "api.example.com"}
	})
	recs := BuildDNSRecords(svc, []string{"203.0.113.10", "2001:db8::1", "203.0.113.11", "2001:db8::2"})
	if len(recs) != 2 {
		t.Fatalf("expected A + AAAA, got %d records: %+v", len(recs), recs)
	}
	byType := map[string]*bulbv1alpha1.DNSRecord{}
	for _, r := range recs {
		byType[r.Spec.Type] = r
	}
	a, ok := byType["A"]
	if !ok {
		t.Fatal("expected an A record")
	}
	if a.Name != "bulb-demo-echo" {
		t.Fatalf("A name: got %q", a.Name)
	}
	if !sameStringSet(a.Spec.Targets, []string{"203.0.113.10", "203.0.113.11"}) {
		t.Fatalf("A targets: got %v", a.Spec.Targets)
	}
	aaaa, ok := byType["AAAA"]
	if !ok {
		t.Fatal("expected an AAAA record")
	}
	if aaaa.Name != "bulb-demo-echo-aaaa" {
		t.Fatalf("AAAA name: got %q", aaaa.Name)
	}
	if !sameStringSet(aaaa.Spec.Targets, []string{"2001:db8::1", "2001:db8::2"}) {
		t.Fatalf("AAAA targets: got %v", aaaa.Spec.Targets)
	}
}

func TestBuildDNSRecords_OnlyAAAA(t *testing.T) {
	svc := mkSvc(func(s *corev1.Service) {
		s.Annotations = map[string]string{AnnotationDNSName: "api.example.com"}
	})
	recs := BuildDNSRecords(svc, []string{"2001:db8::1"})
	if len(recs) != 1 {
		t.Fatalf("expected 1 record, got %d", len(recs))
	}
	if recs[0].Spec.Type != "AAAA" || recs[0].Name != "bulb-demo-echo-aaaa" {
		t.Fatalf("unexpected record: %+v", recs[0])
	}
}

func TestBuildDNSRecords_DoesNotMutateInput(t *testing.T) {
	svc := mkSvc(func(s *corev1.Service) {
		s.Annotations = map[string]string{AnnotationDNSName: "svc.example.com"}
	})
	ips := []string{"10.0.0.3", "10.0.0.1"}
	original := append([]string(nil), ips...)
	_ = BuildDNSRecords(svc, ips)
	if !sameStringSet(ips, original) {
		t.Fatalf("BuildDNSRecords mutated the input slice: got %v want %v", ips, original)
	}
}

func TestApplyDNSRecord_CreatesRecord(t *testing.T) {
	svc := mkSvc(func(s *corev1.Service) {
		s.Annotations = map[string]string{AnnotationDNSName: "api.example.com"}
	})
	nodes := []client.Object{nodeWithIP("a", "1.2.3.4"), nodeWithIP("b", "5.6.7.8")}
	r, c := newReconciler(t, append([]client.Object{svc}, nodes...)...)

	reconcileOnce(t, r, svc.Namespace, svc.Name)

	var rec bulbv1alpha1.DNSRecord
	if err := c.Get(context.Background(), types.NamespacedName{Name: "bulb-demo-echo"}, &rec); err != nil {
		t.Fatalf("expected DNSRecord to exist: %v", err)
	}
	if rec.Spec.FQDN != "api.example.com" {
		t.Fatalf("fqdn: got %q", rec.Spec.FQDN)
	}
	if !sameStringSet(rec.Spec.Targets, []string{"1.2.3.4", "5.6.7.8"}) {
		t.Fatalf("targets: got %v", rec.Spec.Targets)
	}
}

func TestApplyDNSRecord_DeletesRecordWhenAnnotationRemoved(t *testing.T) {
	svc := mkSvc(func(s *corev1.Service) {
		s.Annotations = map[string]string{AnnotationDNSName: "api.example.com"}
	})
	nodes := []client.Object{nodeWithIP("a", "1.2.3.4")}
	r, c := newReconciler(t, append([]client.Object{svc}, nodes...)...)

	reconcileOnce(t, r, svc.Namespace, svc.Name)

	var rec bulbv1alpha1.DNSRecord
	if err := c.Get(context.Background(), types.NamespacedName{Name: "bulb-demo-echo"}, &rec); err != nil {
		t.Fatalf("expected DNSRecord after first reconcile: %v", err)
	}

	var live corev1.Service
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: svc.Namespace, Name: svc.Name}, &live); err != nil {
		t.Fatalf("get service: %v", err)
	}
	delete(live.Annotations, AnnotationDNSName)
	if err := c.Update(context.Background(), &live); err != nil {
		t.Fatalf("update service: %v", err)
	}

	reconcileOnce(t, r, svc.Namespace, svc.Name)

	err := c.Get(context.Background(), types.NamespacedName{Name: "bulb-demo-echo"}, &rec)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected DNSRecord to be cleaned up after annotation removal, got err=%v", err)
	}
}

func TestApplyDNSRecord_UpdatesTargetsOnNodeIPChange(t *testing.T) {
	svc := mkSvc(func(s *corev1.Service) {
		s.Annotations = map[string]string{AnnotationDNSName: "api.example.com"}
	})
	nodes := []client.Object{nodeWithIP("a", "1.2.3.4")}
	r, c := newReconciler(t, append([]client.Object{svc}, nodes...)...)

	reconcileOnce(t, r, svc.Namespace, svc.Name)

	var rec bulbv1alpha1.DNSRecord
	if err := c.Get(context.Background(), types.NamespacedName{Name: "bulb-demo-echo"}, &rec); err != nil {
		t.Fatalf("get dnsrecord: %v", err)
	}
	if !sameStringSet(rec.Spec.Targets, []string{"1.2.3.4"}) {
		t.Fatalf("targets: got %v", rec.Spec.Targets)
	}

	var liveNode corev1.Node
	if err := c.Get(context.Background(), types.NamespacedName{Name: "a"}, &liveNode); err != nil {
		t.Fatalf("get node: %v", err)
	}
	liveNode.Annotations["bulb.toturi.tech/public-ipv4"] = "5.6.7.8"
	if err := c.Update(context.Background(), &liveNode); err != nil {
		t.Fatalf("update node: %v", err)
	}

	reconcileOnce(t, r, svc.Namespace, svc.Name)

	if err := c.Get(context.Background(), types.NamespacedName{Name: "bulb-demo-echo"}, &rec); err != nil {
		t.Fatalf("get dnsrecord after update: %v", err)
	}
	if !sameStringSet(rec.Spec.Targets, []string{"5.6.7.8"}) {
		t.Fatalf("targets after IP change: got %v", rec.Spec.Targets)
	}
}

func TestApplyDNSRecord_NoRecordWithoutAnnotation(t *testing.T) {
	svc := mkSvc()
	nodes := []client.Object{nodeWithIP("a", "1.2.3.4")}
	r, c := newReconciler(t, append([]client.Object{svc}, nodes...)...)

	reconcileOnce(t, r, svc.Namespace, svc.Name)

	var list bulbv1alpha1.DNSRecordList
	if err := c.List(context.Background(), &list); err != nil {
		t.Fatalf("list dnsrecords: %v", err)
	}
	if len(list.Items) != 0 {
		t.Fatalf("expected no DNSRecords without dns-name annotation, got %d", len(list.Items))
	}
}

func TestCleanupDNSRecordsByName(t *testing.T) {
	svc := mkSvc(func(s *corev1.Service) {
		s.Annotations = map[string]string{AnnotationDNSName: "api.example.com"}
	})
	nodes := []client.Object{nodeWithIP("a", "1.2.3.4")}
	r, c := newReconciler(t, append([]client.Object{svc}, nodes...)...)

	reconcileOnce(t, r, svc.Namespace, svc.Name)

	if err := r.cleanupDNSRecordsByName(context.Background(), svc.Namespace, svc.Name); err != nil {
		t.Fatalf("cleanup: %v", err)
	}

	var rec bulbv1alpha1.DNSRecord
	err := c.Get(context.Background(), types.NamespacedName{Name: "bulb-demo-echo"}, &rec)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected DNSRecord to be deleted, got err=%v", err)
	}
}

func TestReconcile_CleansUpDNSRecordOnServiceDelete(t *testing.T) {
	svc := mkSvc(func(s *corev1.Service) {
		s.Annotations = map[string]string{AnnotationDNSName: "api.example.com"}
	})
	nodes := []client.Object{nodeWithIP("a", "1.2.3.4")}
	r, c := newReconciler(t, append([]client.Object{svc}, nodes...)...)

	reconcileOnce(t, r, svc.Namespace, svc.Name)

	var rec bulbv1alpha1.DNSRecord
	if err := c.Get(context.Background(), types.NamespacedName{Name: "bulb-demo-echo"}, &rec); err != nil {
		t.Fatalf("expected DNSRecord after first reconcile: %v", err)
	}

	if err := c.Delete(context.Background(), svc); err != nil {
		t.Fatalf("delete service: %v", err)
	}
	reconcileOnce(t, r, svc.Namespace, svc.Name)

	err := c.Get(context.Background(), types.NamespacedName{Name: "bulb-demo-echo"}, &rec)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected DNSRecord to be cleaned up after Service deletion, got err=%v", err)
	}
}

func TestDNSRecordName(t *testing.T) {
	got := DNSRecordName(mkSvc())
	if got != "bulb-demo-echo" {
		t.Fatalf("got %q", got)
	}
}

func TestReconcile_CleansUpDNSRecordOnTypeChange(t *testing.T) {
	svc := mkSvc(func(s *corev1.Service) {
		s.Annotations = map[string]string{AnnotationDNSName: "api.example.com"}
	})
	nodes := []client.Object{nodeWithIP("a", "1.2.3.4")}
	r, c := newReconciler(t, append([]client.Object{svc}, nodes...)...)

	reconcileOnce(t, r, svc.Namespace, svc.Name)

	var live corev1.Service
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: svc.Namespace, Name: svc.Name}, &live); err != nil {
		t.Fatalf("get service: %v", err)
	}
	live.Spec.Type = corev1.ServiceTypeClusterIP
	if err := c.Update(context.Background(), &live); err != nil {
		t.Fatalf("update: %v", err)
	}

	reconcileOnce(t, r, svc.Namespace, svc.Name)

	var rec bulbv1alpha1.DNSRecord
	err := c.Get(context.Background(), types.NamespacedName{Name: "bulb-demo-echo"}, &rec)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected DNSRecord to be cleaned up on type change, got err=%v", err)
	}
}

func TestReconcile_DNSRecordWithMultiplePorts(t *testing.T) {
	svc := mkSvc(func(s *corev1.Service) {
		s.Annotations = map[string]string{AnnotationDNSName: "api.example.com"}
		s.Spec.Ports = []corev1.ServicePort{
			{Name: "https", Port: 8443, TargetPort: intstr.FromInt32(8443), Protocol: corev1.ProtocolTCP},
			{Name: "http", Port: 8080, TargetPort: intstr.FromInt32(8080), Protocol: corev1.ProtocolTCP},
		}
	})
	nodes := []client.Object{nodeWithIP("a", "1.2.3.4")}
	r, c := newReconciler(t, append([]client.Object{svc}, nodes...)...)

	reconcileOnce(t, r, svc.Namespace, svc.Name)

	var rec bulbv1alpha1.DNSRecord
	if err := c.Get(context.Background(), types.NamespacedName{Name: "bulb-demo-echo"}, &rec); err != nil {
		t.Fatalf("expected single DNSRecord for multi-port service: %v", err)
	}
	if rec.Spec.FQDN != "api.example.com" {
		t.Fatalf("fqdn: got %q", rec.Spec.FQDN)
	}
}

func TestApplyDNSRecord_CreatesBothFamiliesForDualStackNodes(t *testing.T) {
	svc := mkSvc(func(s *corev1.Service) {
		s.Annotations = map[string]string{AnnotationDNSName: "api.example.com"}
	})
	nodes := []client.Object{
		nodeWithIPs("a", "1.2.3.4", "2001:db8::1"),
		nodeWithIPs("b", "5.6.7.8", "2001:db8::2"),
	}
	r, c := newReconciler(t, append([]client.Object{svc}, nodes...)...)

	reconcileOnce(t, r, svc.Namespace, svc.Name)

	a := getDNSRecord(t, c, "bulb-demo-echo")
	if a.Spec.Type != "A" {
		t.Fatalf("A record: got type %q", a.Spec.Type)
	}
	if !sameStringSet(a.Spec.Targets, []string{"1.2.3.4", "5.6.7.8"}) {
		t.Fatalf("A targets: got %v", a.Spec.Targets)
	}
	aaaa := getDNSRecord(t, c, "bulb-demo-echo-aaaa")
	if aaaa.Spec.Type != "AAAA" {
		t.Fatalf("AAAA record: got type %q", aaaa.Spec.Type)
	}
	if !sameStringSet(aaaa.Spec.Targets, []string{"2001:db8::1", "2001:db8::2"}) {
		t.Fatalf("AAAA targets: got %v", aaaa.Spec.Targets)
	}
}

func TestApplyDNSRecord_PrunesAAAAWhenIPv6Disappears(t *testing.T) {
	svc := mkSvc(func(s *corev1.Service) {
		s.Annotations = map[string]string{AnnotationDNSName: "api.example.com"}
	})
	nodes := []client.Object{nodeWithIPs("a", "1.2.3.4", "2001:db8::1")}
	r, c := newReconciler(t, append([]client.Object{svc}, nodes...)...)

	reconcileOnce(t, r, svc.Namespace, svc.Name)

	if _, err := getDNSRecordOrErr(c, "bulb-demo-echo-aaaa"); err != nil {
		t.Fatalf("expected AAAA after first reconcile: %v", err)
	}

	var liveNode corev1.Node
	if err := c.Get(context.Background(), types.NamespacedName{Name: "a"}, &liveNode); err != nil {
		t.Fatalf("get node: %v", err)
	}
	delete(liveNode.Annotations, "bulb.toturi.tech/public-ipv6")
	if err := c.Update(context.Background(), &liveNode); err != nil {
		t.Fatalf("update node: %v", err)
	}

	reconcileOnce(t, r, svc.Namespace, svc.Name)

	var rec bulbv1alpha1.DNSRecord
	err := c.Get(context.Background(), types.NamespacedName{Name: "bulb-demo-echo-aaaa"}, &rec)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected AAAA record to be pruned, got err=%v", err)
	}
	if _, err := getDNSRecordOrErr(c, "bulb-demo-echo"); err != nil {
		t.Fatalf("A record should still exist: %v", err)
	}
}

func getDNSRecordOrErr(c client.Client, name string) (*bulbv1alpha1.DNSRecord, error) {
	var rec bulbv1alpha1.DNSRecord
	if err := c.Get(context.Background(), types.NamespacedName{Name: name}, &rec); err != nil {
		return nil, err
	}
	return &rec, nil
}

func getDNSRecord(t *testing.T, c client.Client, name string) *bulbv1alpha1.DNSRecord {
	t.Helper()
	var rec bulbv1alpha1.DNSRecord
	if err := c.Get(context.Background(), types.NamespacedName{Name: name}, &rec); err != nil {
		t.Fatalf("get dnsrecord %s: %v", name, err)
	}
	return &rec
}
