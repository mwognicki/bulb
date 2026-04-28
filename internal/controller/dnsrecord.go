package controller

import (
	"context"
	"fmt"
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
	defaultDNSType    = "A"
	defaultDNSTTL     = 60
)

// BuildDNSRecord renders the desired DNSRecord for a Service. Returns
// nil if the Service has no bulb.toturi.tech/dns-name annotation.
func BuildDNSRecord(svc *corev1.Service, publicIPs []string) *bulbv1alpha1.DNSRecord {
	fqdn := svc.Annotations[AnnotationDNSName]
	if fqdn == "" {
		return nil
	}

	targets := append([]string(nil), publicIPs...)
	sort.Strings(targets)

	return &bulbv1alpha1.DNSRecord{
		TypeMeta: metav1.TypeMeta{
			APIVersion: bulbv1alpha1.GroupVersion.String(),
			Kind:       "DNSRecord",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:   DNSRecordName(svc),
			Labels: serviceLabels(svc),
		},
		Spec: bulbv1alpha1.DNSRecordSpec{
			FQDN:    fqdn,
			Type:    defaultDNSType,
			TTL:     defaultDNSTTL,
			Targets: targets,
		},
	}
}

func DNSRecordName(svc *corev1.Service) string {
	return fmt.Sprintf("bulb-%s-%s", svc.Namespace, svc.Name)
}

func (r *ServiceReconciler) applyDNSRecord(ctx context.Context, desired *bulbv1alpha1.DNSRecord, svc *corev1.Service) error {
	if desired == nil {
		return r.cleanupDNSRecord(ctx, svc)
	}

	logger := log.FromContext(ctx).WithValues("dnsrecord", desired.Name, "fqdn", desired.Spec.FQDN)

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

func (r *ServiceReconciler) cleanupDNSRecord(ctx context.Context, svc *corev1.Service) error {
	var rec bulbv1alpha1.DNSRecord
	err := r.Get(ctx, types.NamespacedName{Name: DNSRecordName(svc)}, &rec)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get dnsrecord for cleanup: %w", err)
	}
	if rec.Labels[labelService] != svc.Name || rec.Labels[labelServiceNs] != svc.Namespace || rec.Labels[labelManagedBy] != labelManagedByV {
		return nil
	}
	if err := r.Delete(ctx, &rec); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete dnsrecord: %w", err)
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

func logDNSConfig(ctx context.Context, svc *corev1.Service, rec *bulbv1alpha1.DNSRecord) {
	if rec == nil {
		return
	}
	logger := log.FromContext(ctx)
	logger.Info("desired DNS configuration",
		"service", fmt.Sprintf("%s/%s", svc.Namespace, svc.Name),
		"fqdn", rec.Spec.FQDN,
		"type", rec.Spec.Type,
		"ttl", rec.Spec.TTL,
		"targets", strings.Join(rec.Spec.Targets, ","),
	)
}
