package controller

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	bulbv1alpha1 "github.com/mwognicki/bulb/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	lbPortOwnerName = "bulb-controller"
	allNodesToken   = "*"
)

// BuildLBPorts renders the desired LBPort objects for a Service.
func (r *ServiceReconciler) BuildLBPorts(ctx context.Context, svc *corev1.Service) ([]bulbv1alpha1.LBPort, error) {
	nodes, err := r.lbPortNodes(ctx, svc)
	if err != nil {
		return nil, err
	}

	ports := make([]bulbv1alpha1.LBPort, 0, len(svc.Spec.Ports))
	for _, p := range svc.Spec.Ports {
		protocol := p.Protocol
		if protocol == "" {
			protocol = corev1.ProtocolTCP
		}
		ports = append(ports, bulbv1alpha1.LBPort{
			TypeMeta: metav1.TypeMeta{
				APIVersion: bulbv1alpha1.GroupVersion.String(),
				Kind:       "LBPort",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:   LBPortName(svc, p),
				Labels: serviceLabels(svc),
			},
			Spec: bulbv1alpha1.LBPortSpec{
				Port:     p.Port,
				Protocol: protocol,
				Nodes:    append([]string(nil), nodes...),
				Owner:    lbPortOwnerName,
			},
		})
	}
	return ports, nil
}

func LBPortName(svc *corev1.Service, port corev1.ServicePort) string {
	protocol := strings.ToLower(string(port.Protocol))
	if protocol == "" {
		protocol = strings.ToLower(string(corev1.ProtocolTCP))
	}
	return fmt.Sprintf("bulb-%s-%s-%d-%s", svc.Namespace, svc.Name, port.Port, protocol)
}

func (r *ServiceReconciler) applyLBPorts(ctx context.Context, desired []bulbv1alpha1.LBPort, svc *corev1.Service) error {
	keep := make(map[string]struct{}, len(desired))
	for _, want := range desired {
		keep[want.Name] = struct{}{}

		var existing bulbv1alpha1.LBPort
		err := r.Get(ctx, types.NamespacedName{Name: want.Name}, &existing)
		if apierrors.IsNotFound(err) {
			if err := r.Create(ctx, want.DeepCopy()); err != nil {
				return fmt.Errorf("create lbport %s: %w", want.Name, err)
			}
			continue
		}
		if err != nil {
			return fmt.Errorf("get lbport %s: %w", want.Name, err)
		}

		if equality.Semantic.DeepEqual(existing.Spec, want.Spec) &&
			equality.Semantic.DeepEqual(existing.Labels, want.Labels) {
			continue
		}

		existing.Spec = want.Spec
		existing.Labels = want.Labels
		if err := r.Update(ctx, &existing); err != nil {
			return fmt.Errorf("update lbport %s: %w", want.Name, err)
		}
	}

	var existing bulbv1alpha1.LBPortList
	if err := r.List(ctx, &existing, client.MatchingLabels(serviceLabels(svc))); err != nil {
		return fmt.Errorf("list lbports: %w", err)
	}
	for _, item := range existing.Items {
		if _, ok := keep[item.Name]; ok {
			continue
		}
		if err := r.Delete(ctx, item.DeepCopy()); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete stale lbport %s: %w", item.Name, err)
		}
	}
	return nil
}

func (r *ServiceReconciler) cleanupLBPorts(ctx context.Context, svcNamespace, svcName string) error {
	var list bulbv1alpha1.LBPortList
	if err := r.List(ctx, &list, client.MatchingLabels(map[string]string{
		labelManagedBy: labelManagedByV,
		labelService:   svcName,
		labelServiceNs: svcNamespace,
	})); err != nil {
		return fmt.Errorf("list lbports for cleanup: %w", err)
	}
	for _, item := range list.Items {
		if err := r.Delete(ctx, item.DeepCopy()); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete lbport %s: %w", item.Name, err)
		}
	}
	return nil
}

func (r *ServiceReconciler) lbPortNodes(ctx context.Context, svc *corev1.Service) ([]string, error) {
	rawSelector := svc.Annotations[AnnotationNodes]
	if rawSelector == "" {
		return []string{allNodesToken}, nil
	}

	selectorSpec, err := metav1.ParseToLabelSelector(rawSelector)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", AnnotationNodes, err)
	}
	selector, err := metav1.LabelSelectorAsSelector(selectorSpec)
	if err != nil {
		return nil, fmt.Errorf("convert %s: %w", AnnotationNodes, err)
	}

	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes); err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}

	matched := make([]string, 0, len(nodes.Items))
	for _, node := range nodes.Items {
		if node.Spec.Unschedulable {
			continue
		}
		if selector.Matches(labels.Set(node.Labels)) {
			matched = append(matched, node.Name)
		}
	}
	sort.Strings(matched)
	return matched, nil
}

func serviceLabels(svc *corev1.Service) map[string]string {
	return map[string]string{
		labelManagedBy: labelManagedByV,
		labelService:   svc.Name,
		labelServiceNs: svc.Namespace,
	}
}

func names(items []bulbv1alpha1.LBPort) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, item.Name)
	}
	sort.Strings(out)
	return out
}

func sameStringSet(a, b []string) bool {
	aa := append([]string(nil), a...)
	bb := append([]string(nil), b...)
	sort.Strings(aa)
	sort.Strings(bb)
	return slices.Equal(aa, bb)
}
