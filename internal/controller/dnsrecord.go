package controller

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strings"

	bulbv1alpha1 "github.com/mwognicki/bulb/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	AnnotationDNSName = "bulb.toturi.tech/dns-name"
	dnsTypeA          = "A"
	dnsTypeAAAA       = "AAAA"
	defaultDNSTTL     = 60
)

// BuildDNSRecords renders the desired DNS records for a Service. v4
// targets become an A record (`bulb-<ns>-<name>`); v6 targets become an
// AAAA record (`bulb-<ns>-<name>-aaaa`). A family with no targets gets
// no record. Returns nil if the Service has no
// bulb.toturi.tech/dns-name annotation.
func BuildDNSRecords(svc *corev1.Service, publicIPs []string) []*bulbv1alpha1.DNSRecord {
	fqdn := svc.Annotations[AnnotationDNSName]
	if fqdn == "" {
		return nil
	}

	v4, v6 := splitByFamily(publicIPs)
	var out []*bulbv1alpha1.DNSRecord
	if rec := buildDNSRecord(svc, fqdn, dnsTypeA, DNSRecordName(svc), v4); rec != nil {
		out = append(out, rec)
	}
	if rec := buildDNSRecord(svc, fqdn, dnsTypeAAAA, DNSRecordNameAAAA(svc), v6); rec != nil {
		out = append(out, rec)
	}
	// When neither family has targets we still want a single empty A
	// record so the dry-run signal ("operator asked for DNS but bulb
	// found no targets") doesn't disappear silently.
	if len(out) == 0 {
		out = append(out, buildDNSRecordAlways(svc, fqdn, dnsTypeA, DNSRecordName(svc), nil))
	}
	return out
}

func buildDNSRecord(svc *corev1.Service, fqdn, recType, name string, targets []string) *bulbv1alpha1.DNSRecord {
	if len(targets) == 0 {
		return nil
	}
	return buildDNSRecordAlways(svc, fqdn, recType, name, targets)
}

func buildDNSRecordAlways(svc *corev1.Service, fqdn, recType, name string, targets []string) *bulbv1alpha1.DNSRecord {
	t := append([]string(nil), targets...)
	sort.Strings(t)
	return &bulbv1alpha1.DNSRecord{
		TypeMeta: metav1.TypeMeta{
			APIVersion: bulbv1alpha1.GroupVersion.String(),
			Kind:       "DNSRecord",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: serviceLabels(svc),
		},
		Spec: bulbv1alpha1.DNSRecordSpec{
			FQDN:    fqdn,
			Type:    recType,
			TTL:     defaultDNSTTL,
			Targets: t,
		},
	}
}

func DNSRecordName(svc *corev1.Service) string {
	return fmt.Sprintf("bulb-%s-%s", svc.Namespace, svc.Name)
}

// DNSRecordNameAAAA is the deterministic name for the AAAA record, kept
// distinct from the A record so both can coexist as cluster-scoped
// objects.
func DNSRecordNameAAAA(svc *corev1.Service) string {
	return DNSRecordName(svc) + "-aaaa"
}

func splitByFamily(ips []string) (v4, v6 []string) {
	for _, raw := range ips {
		ip := net.ParseIP(raw)
		if ip == nil {
			continue
		}
		if ip.To4() != nil {
			v4 = append(v4, raw)
		} else {
			v6 = append(v6, raw)
		}
	}
	return v4, v6
}

func (r *ServiceReconciler) applyDNSRecords(ctx context.Context, desired []*bulbv1alpha1.DNSRecord, svc *corev1.Service) error {
	wanted := make(map[string]struct{}, len(desired))
	for _, rec := range desired {
		wanted[rec.Name] = struct{}{}
		if err := r.applyOneDNSRecord(ctx, rec); err != nil {
			return err
		}
	}
	// Remove any of our records that were created previously but are no
	// longer wanted (e.g. v6 nodes drained, or annotation removed).
	return r.pruneUnwantedDNSRecords(ctx, svc, wanted)
}

func (r *ServiceReconciler) applyOneDNSRecord(ctx context.Context, desired *bulbv1alpha1.DNSRecord) error {
	logger := log.FromContext(ctx).WithValues("dnsrecord", desired.Name, "fqdn", desired.Spec.FQDN, "type", desired.Spec.Type)

	var existing bulbv1alpha1.DNSRecord
	err := r.Get(ctx, types.NamespacedName{Name: desired.Name}, &existing)
	if apierrors.IsNotFound(err) {
		if err := r.Create(ctx, desired.DeepCopy()); err != nil {
			return fmt.Errorf("create dnsrecord: %w", err)
		}
		logger.Info("DNS record created (dry-run)", "targets", desired.Spec.Targets, "ttl", desired.Spec.TTL)
		return nil
	}
	if err != nil {
		return fmt.Errorf("get dnsrecord: %w", err)
	}

	if equality.Semantic.DeepEqual(existing.Spec, desired.Spec) &&
		equality.Semantic.DeepEqual(existing.Labels, desired.Labels) {
		return nil
	}

	existing.Spec = desired.Spec
	existing.Labels = desired.Labels
	if err := r.Update(ctx, &existing); err != nil {
		return fmt.Errorf("update dnsrecord: %w", err)
	}
	logger.Info("DNS record updated (dry-run)", "targets", desired.Spec.Targets, "ttl", desired.Spec.TTL)
	return nil
}

func (r *ServiceReconciler) pruneUnwantedDNSRecords(ctx context.Context, svc *corev1.Service, wanted map[string]struct{}) error {
	var list bulbv1alpha1.DNSRecordList
	if err := r.List(ctx, &list, client.MatchingLabels(map[string]string{
		labelManagedBy: labelManagedByV,
		labelService:   svc.Name,
		labelServiceNs: svc.Namespace,
	})); err != nil {
		return fmt.Errorf("list dnsrecords for prune: %w", err)
	}
	for _, item := range list.Items {
		if _, keep := wanted[item.Name]; keep {
			continue
		}
		if err := r.Delete(ctx, item.DeepCopy()); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete stale dnsrecord %s: %w", item.Name, err)
		}
	}
	return nil
}

func (r *ServiceReconciler) cleanupDNSRecordsByName(ctx context.Context, svcNamespace, svcName string) error {
	var list bulbv1alpha1.DNSRecordList
	if err := r.List(ctx, &list, client.MatchingLabels(map[string]string{
		labelManagedBy: labelManagedByV,
		labelService:   svcName,
		labelServiceNs: svcNamespace,
	})); err != nil {
		return fmt.Errorf("list dnsrecords for cleanup: %w", err)
	}
	for _, item := range list.Items {
		if err := r.Delete(ctx, item.DeepCopy()); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete dnsrecord %s: %w", item.Name, err)
		}
	}
	return nil
}

func logDNSConfig(ctx context.Context, svc *corev1.Service, recs []*bulbv1alpha1.DNSRecord) {
	if len(recs) == 0 {
		return
	}
	logger := log.FromContext(ctx)
	for _, rec := range recs {
		logger.Info("desired DNS configuration",
			"service", fmt.Sprintf("%s/%s", svc.Namespace, svc.Name),
			"fqdn", rec.Spec.FQDN,
			"type", rec.Spec.Type,
			"ttl", rec.Spec.TTL,
			"targets", strings.Join(rec.Spec.Targets, ","),
		)
	}
}
