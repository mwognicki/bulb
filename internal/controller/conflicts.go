package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"

	bulbv1alpha1 "github.com/mwognicki/bulb/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type portConflict struct {
	Reason  string
	Message string
}

func (r *ServiceReconciler) detectPortConflict(ctx context.Context, svc *corev1.Service, desired []bulbv1alpha1.LBPort) (*portConflict, error) {
	if conflict := duplicateServicePortConflict(svc, desired); conflict != nil {
		return conflict, nil
	}
	if conflict, err := r.lbPortOwnerConflict(ctx, desired); conflict != nil || err != nil {
		return conflict, err
	}
	return r.servicePortConflict(ctx, svc, desired)
}

func duplicateServicePortConflict(svc *corev1.Service, desired []bulbv1alpha1.LBPort) *portConflict {
	seen := make(map[string]struct{}, len(desired))
	for _, lbport := range desired {
		key := portKey(lbport.Spec.Port, lbport.Spec.Protocol)
		if _, ok := seen[key]; ok {
			return &portConflict{
				Reason:  "DuplicateServicePort",
				Message: fmt.Sprintf("Service %s/%s declares port %s more than once", svc.Namespace, svc.Name, key),
			}
		}
		seen[key] = struct{}{}
	}
	return nil
}

func (r *ServiceReconciler) lbPortOwnerConflict(ctx context.Context, desired []bulbv1alpha1.LBPort) (*portConflict, error) {
	for _, want := range desired {
		var existing bulbv1alpha1.LBPort
		err := r.Get(ctx, types.NamespacedName{Name: want.Name}, &existing)
		if client.IgnoreNotFound(err) != nil {
			return nil, fmt.Errorf("get lbport %s for conflict detection: %w", want.Name, err)
		}
		if err != nil {
			continue
		}
		if existing.Spec.Owner != "" && existing.Spec.Owner != lbPortOwnerName {
			return &portConflict{
				Reason:  "LBPortOwnerConflict",
				Message: fmt.Sprintf("LBPort %s is owned by %q, not %q", existing.Name, existing.Spec.Owner, lbPortOwnerName),
			}, nil
		}
		if !sameStringMap(existing.Labels, want.Labels) {
			return &portConflict{
				Reason:  "LBPortOwnerConflict",
				Message: fmt.Sprintf("LBPort %s already exists for another Service or manager", existing.Name),
			}, nil
		}
	}
	return nil, nil
}

func (r *ServiceReconciler) servicePortConflict(ctx context.Context, svc *corev1.Service, desired []bulbv1alpha1.LBPort) (*portConflict, error) {
	var services corev1.ServiceList
	if err := r.List(ctx, &services); err != nil {
		return nil, fmt.Errorf("list services for conflict detection: %w", err)
	}

	for i := range services.Items {
		other := &services.Items[i]
		if other.Namespace == svc.Namespace && other.Name == svc.Name {
			continue
		}
		if !ServiceMatches(other) {
			continue
		}
		otherPorts, err := r.BuildLBPorts(ctx, other)
		if err != nil {
			continue
		}
		if conflict := overlappingPortConflict(svc, desired, other, otherPorts); conflict != nil {
			return conflict, nil
		}
	}
	return nil, nil
}

func overlappingPortConflict(svc *corev1.Service, desired []bulbv1alpha1.LBPort, other *corev1.Service, otherPorts []bulbv1alpha1.LBPort) *portConflict {
	for _, left := range desired {
		for _, right := range otherPorts {
			if left.Spec.Port != right.Spec.Port || normalizedProtocol(left.Spec.Protocol) != normalizedProtocol(right.Spec.Protocol) {
				continue
			}
			nodes := overlappingNodes(left.Spec.Nodes, right.Spec.Nodes)
			if len(nodes) == 0 {
				continue
			}
			return &portConflict{
				Reason: "ServicePortConflict",
				Message: fmt.Sprintf(
					"Service %s/%s conflicts with Service %s/%s on %s for node scope %s",
					svc.Namespace,
					svc.Name,
					other.Namespace,
					other.Name,
					portKey(left.Spec.Port, left.Spec.Protocol),
					strings.Join(nodes, ","),
				),
			}
		}
	}
	return nil
}

func overlappingNodes(a, b []string) []string {
	if len(a) == 0 || len(b) == 0 {
		return nil
	}
	if isAllNodes(a) && isAllNodes(b) {
		return []string{allNodesToken}
	}
	if isAllNodes(a) {
		return sortedConcreteNodes(b)
	}
	if isAllNodes(b) {
		return sortedConcreteNodes(a)
	}
	set := make(map[string]struct{}, len(a))
	for _, node := range a {
		set[node] = struct{}{}
	}
	var out []string
	for _, node := range b {
		if _, ok := set[node]; ok {
			out = append(out, node)
		}
	}
	sort.Strings(out)
	return out
}

func sortedConcreteNodes(nodes []string) []string {
	out := make([]string, 0, len(nodes))
	for _, node := range nodes {
		if node != "" && node != allNodesToken {
			out = append(out, node)
		}
	}
	sort.Strings(out)
	return out
}

func isAllNodes(nodes []string) bool {
	return len(nodes) == 1 && nodes[0] == allNodesToken
}

func portKey(port int32, protocol corev1.Protocol) string {
	return fmt.Sprintf("%d/%s", port, strings.ToLower(string(normalizedProtocol(protocol))))
}

func normalizedProtocol(protocol corev1.Protocol) corev1.Protocol {
	if protocol == "" {
		return corev1.ProtocolTCP
	}
	return protocol
}

func sameStringMap(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, av := range a {
		if b[key] != av {
			return false
		}
	}
	return true
}
