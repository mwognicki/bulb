package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	ConditionReconciled      = "Reconciled"
	ConditionPortConflict    = "PortConflict"
	ConditionInvalidService  = "InvalidService"
	ConditionNoReadyEndpoint = "NoReadyEndpoints"
)

func serviceCondition(svc *corev1.Service, typ string, status metav1.ConditionStatus, reason, message string) metav1.Condition {
	return metav1.Condition{
		Type:               typ,
		Status:             status,
		ObservedGeneration: svc.Generation,
		Reason:             reason,
		Message:            message,
	}
}

func (r *ServiceReconciler) updateStatus(ctx context.Context, svc *corev1.Service, ips []string, conditions ...metav1.Condition) error {
	desiredIngress := make([]corev1.LoadBalancerIngress, 0, len(ips))
	for _, ip := range ips {
		desiredIngress = append(desiredIngress, corev1.LoadBalancerIngress{IP: ip})
	}

	patched := svc.DeepCopy()
	patched.Status.LoadBalancer.Ingress = desiredIngress
	for _, condition := range conditions {
		apimeta.SetStatusCondition(&patched.Status.Conditions, condition)
	}

	if equality.Semantic.DeepEqual(svc.Status.LoadBalancer.Ingress, patched.Status.LoadBalancer.Ingress) &&
		equality.Semantic.DeepEqual(svc.Status.Conditions, patched.Status.Conditions) {
		return nil
	}
	return r.Status().Patch(ctx, patched, client.MergeFrom(svc))
}
