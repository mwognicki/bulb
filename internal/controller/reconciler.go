package controller

import (
	"context"
	"fmt"
	"net"
	"sort"

	bulbv1alpha1 "github.com/mwognicki/bulb/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// LoadBalancerClass is the spec.loadBalancerClass value bulb claims.
const LoadBalancerClass = "bulb"

// ServiceReconciler reconciles Services with type=LoadBalancer and
// loadBalancerClass empty or "bulb" into per-Service proxy DaemonSets.
type ServiceReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	record.EventRecorder

	Namespace string
	Image     string
}

// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=services/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core,resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=endpoints,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=daemonsets,verbs=get;list;watch;create;update;patch;delete

// Reconcile implements the controller-runtime Reconciler interface.
func (r *ServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("service", req.NamespacedName)

	var svc corev1.Service
	if err := r.Get(ctx, req.NamespacedName, &svc); err != nil {
		if apierrors.IsNotFound(err) {
			// Service is gone; clean up the DS we created for it. Cross-namespace
			// OwnerReferences are disallowed for namespaced kinds, so we can't
			// rely on Kubernetes GC.
			return r.cleanupByName(ctx, req.Namespace, req.Name)
		}
		return ctrl.Result{}, fmt.Errorf("get service: %w", err)
	}

	if !ServiceMatches(&svc) {
		return r.cleanupByName(ctx, svc.Namespace, svc.Name)
	}

	endpoints, err := r.serviceEndpoints(ctx, &svc)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("read service endpoints: %w", err)
	}
	if svc.Annotations[AnnotationExternalTrafficPolicy] == "Local" && endpoints.empty() {
		msg := fmt.Sprintf("Service %s/%s uses externalTrafficPolicy=Local but has no ready endpoints", svc.Namespace, svc.Name)
		r.eventf(&svc, corev1.EventTypeWarning, "NoReadyEndpoints", msg)
		if err := r.updateStatus(ctx, &svc, nil,
			serviceCondition(&svc, ConditionNoReadyEndpoint, metav1.ConditionTrue, "NoReadyEndpoints", msg),
			serviceCondition(&svc, ConditionReconciled, metav1.ConditionFalse, "NoReadyEndpoints", msg),
			serviceCondition(&svc, ConditionPortConflict, metav1.ConditionFalse, "NoConflict", "No port conflict detected"),
			serviceCondition(&svc, ConditionInvalidService, metav1.ConditionFalse, "Valid", "Service annotations and ports are valid"),
		); err != nil {
			return ctrl.Result{}, fmt.Errorf("update no-ready-endpoints condition: %w", err)
		}
		return r.cleanupByName(ctx, svc.Namespace, svc.Name)
	}

	lbports, err := r.BuildLBPorts(ctx, &svc)
	if err != nil {
		return r.markInvalidService(ctx, &svc, "InvalidAnnotation", err)
	}

	conflict, err := r.detectPortConflict(ctx, &svc, lbports)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("detect port conflicts: %w", err)
	}
	if conflict != nil {
		r.eventf(&svc, corev1.EventTypeWarning, conflict.Reason, conflict.Message)
		if err := r.updateStatus(ctx, &svc, nil,
			serviceCondition(&svc, ConditionPortConflict, metav1.ConditionTrue, conflict.Reason, conflict.Message),
			serviceCondition(&svc, ConditionReconciled, metav1.ConditionFalse, conflict.Reason, conflict.Message),
			serviceCondition(&svc, ConditionInvalidService, metav1.ConditionFalse, "Valid", "Service annotations and ports are valid"),
			serviceCondition(&svc, ConditionNoReadyEndpoint, metav1.ConditionFalse, "ReadyEndpointsFound", "Ready endpoints are available or not required"),
		); err != nil {
			return ctrl.Result{}, fmt.Errorf("update port conflict condition: %w", err)
		}
		return r.cleanupByName(ctx, svc.Namespace, svc.Name)
	}

	desired, err := BuildDaemonSet(&svc, r.Image, r.Namespace, endpoints)
	if err != nil {
		logger.Error(err, "build daemonset failed")
		return r.markInvalidService(ctx, &svc, "InvalidService", err)
	}

	if err := r.applyDaemonSet(ctx, desired); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.applyLBPorts(ctx, lbports, &svc); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.pruneStaleAppliedNodes(ctx, &svc); err != nil {
		logger.Error(err, "prune stale applied nodes")
	}
	r.emitLBPortEvents(ctx, &svc)

	ips, err := r.nodePublicIPs(ctx)
	if err != nil {
		logger.Error(err, "read node public IPs; status will not be updated this round")
		return ctrl.Result{}, err
	}

	dnsRecs := BuildDNSRecords(&svc, ips)
	if err := r.applyDNSRecords(ctx, dnsRecs, &svc); err != nil {
		return ctrl.Result{}, fmt.Errorf("apply dnsrecords: %w", err)
	}
	logDNSConfig(ctx, &svc, dnsRecs)

	if err := r.updateStatus(ctx, &svc, ips,
		serviceCondition(&svc, ConditionReconciled, metav1.ConditionTrue, "Reconciled", "Service reconciled successfully"),
		serviceCondition(&svc, ConditionPortConflict, metav1.ConditionFalse, "NoConflict", "No port conflict detected"),
		serviceCondition(&svc, ConditionInvalidService, metav1.ConditionFalse, "Valid", "Service annotations and ports are valid"),
		serviceCondition(&svc, ConditionNoReadyEndpoint, metav1.ConditionFalse, "ReadyEndpointsFound", "Ready endpoints are available or not required"),
	); err != nil {
		return ctrl.Result{}, fmt.Errorf("update service status: %w", err)
	}
	r.eventf(&svc, corev1.EventTypeNormal, "Reconciled", "Service reconciled successfully")

	return ctrl.Result{}, nil
}

// ServiceMatches reports whether bulb owns this Service's
// LoadBalancer reconciliation.
func ServiceMatches(svc *corev1.Service) bool {
	if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
		return false
	}
	cls := ""
	if svc.Spec.LoadBalancerClass != nil {
		cls = *svc.Spec.LoadBalancerClass
	}
	return cls == "" || cls == LoadBalancerClass
}

// applyDaemonSet creates the desired DS or patches the existing one's
// spec. Equality.Semantic skips noisy churn from defaulting.
func (r *ServiceReconciler) applyDaemonSet(ctx context.Context, desired *appsv1.DaemonSet) error {
	var existing appsv1.DaemonSet
	key := types.NamespacedName{Namespace: desired.Namespace, Name: desired.Name}
	err := r.Get(ctx, key, &existing)
	if apierrors.IsNotFound(err) {
		if err := r.Create(ctx, desired); err != nil {
			return fmt.Errorf("create daemonset: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("get daemonset: %w", err)
	}

	if equality.Semantic.DeepEqual(existing.Spec, desired.Spec) &&
		equality.Semantic.DeepEqual(existing.Labels, desired.Labels) {
		return nil
	}

	existing.Spec = desired.Spec
	existing.Labels = desired.Labels
	if err := r.Update(ctx, &existing); err != nil {
		return fmt.Errorf("update daemonset: %w", err)
	}
	return nil
}

func (r *ServiceReconciler) markInvalidService(ctx context.Context, svc *corev1.Service, reason string, cause error) (ctrl.Result, error) {
	msg := cause.Error()
	r.eventf(svc, corev1.EventTypeWarning, reason, msg)
	if err := r.updateStatus(ctx, svc, nil,
		serviceCondition(svc, ConditionInvalidService, metav1.ConditionTrue, reason, msg),
		serviceCondition(svc, ConditionReconciled, metav1.ConditionFalse, reason, msg),
		serviceCondition(svc, ConditionPortConflict, metav1.ConditionFalse, "NoConflict", "No port conflict detected"),
		serviceCondition(svc, ConditionNoReadyEndpoint, metav1.ConditionFalse, "ReadyEndpointsFound", "Ready endpoints are available or not required"),
	); err != nil {
		return ctrl.Result{}, fmt.Errorf("update invalid service condition: %w", err)
	}
	return r.cleanupByName(ctx, svc.Namespace, svc.Name)
}

func (r *ServiceReconciler) eventf(svc *corev1.Service, eventType, reason, message string) {
	if r.EventRecorder == nil {
		return
	}
	r.Eventf(svc, eventType, reason, "%s", message)
}

// cleanupByName deletes the DaemonSet that BuildDaemonSet would have
// produced for (svcNamespace, svcName). The labels we put on the DS
// pin it to that Service, so we can confirm we own the object before
// deleting.
func (r *ServiceReconciler) cleanupByName(ctx context.Context, svcNamespace, svcName string) (ctrl.Result, error) {
	dsName := fmt.Sprintf("bulb-%s-%s", svcNamespace, svcName)
	var ds appsv1.DaemonSet
	err := r.Get(ctx, types.NamespacedName{Namespace: r.Namespace, Name: dsName}, &ds)
	if apierrors.IsNotFound(err) {
		if err := r.cleanupLBPorts(ctx, svcNamespace, svcName); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.cleanupDNSRecordsByName(ctx, svcNamespace, svcName); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("get daemonset for cleanup: %w", err)
	}
	if ds.Labels[labelService] != svcName || ds.Labels[labelServiceNs] != svcNamespace || ds.Labels[labelManagedBy] != labelManagedByV {
		// Name collision with something we don't own. Refuse to delete.
		return ctrl.Result{}, nil
	}
	if err := r.Delete(ctx, &ds); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("delete daemonset: %w", err)
	}
	if err := r.cleanupLBPorts(ctx, svcNamespace, svcName); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.cleanupDNSRecordsByName(ctx, svcNamespace, svcName); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// nodePublicIPs lists all schedulable nodes and collects their annotated
// public IPs (bulb.toturi.tech/public-ipv4, bulb.toturi.tech/public-ipv6).
// Returns a sorted, deduplicated list.
func (r *ServiceReconciler) nodePublicIPs(ctx context.Context) ([]string, error) {
	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes); err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	seen := make(map[string]struct{})
	ips := make([]string, 0, len(nodes.Items))
	for _, node := range nodes.Items {
		if node.Spec.Unschedulable {
			continue
		}
		for _, ann := range []string{
			"bulb.toturi.tech/public-ipv4",
			"bulb.toturi.tech/public-ipv6",
		} {
			if ip := node.Annotations[ann]; ip != "" {
				if _, ok := seen[ip]; !ok {
					seen[ip] = struct{}{}
					ips = append(ips, ip)
				}
			}
		}
	}
	sort.Strings(ips)
	return ips, nil
}

// SetupWithManager wires the reconciler into the manager and limits
// reconciles to Services we might own (filter at the predicate layer
// to keep the work queue tight).
func (r *ServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Namespace == "" {
		return fmt.Errorf("ServiceReconciler.Namespace is required")
	}
	if r.Image == "" {
		return fmt.Errorf("ServiceReconciler.Image is required")
	}
	if r.Scheme == nil {
		r.Scheme = mgr.GetScheme()
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Service{}, builder.WithPredicates(servicePredicate())).
		Watches(
			&bulbv1alpha1.LBPort{},
			handler.EnqueueRequestsFromMapFunc(lbPortToService),
		).
		Watches(
			&corev1.Node{},
			handler.EnqueueRequestsFromMapFunc(nodeToAllServices(r)),
		).
		Watches(
			&corev1.Endpoints{},
			handler.EnqueueRequestsFromMapFunc(endpointToService),
		).
		Complete(r)
}

func (r *ServiceReconciler) serviceEndpoints(ctx context.Context, svc *corev1.Service) (ServiceEndpoints, error) {
	if svc.Annotations[AnnotationExternalTrafficPolicy] != "Local" {
		return nil, nil
	}

	var eps corev1.Endpoints
	key := types.NamespacedName{Namespace: svc.Namespace, Name: svc.Name}
	if err := r.Get(ctx, key, &eps); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("get endpoints %s: %w", key.String(), err)
	}
	return endpointsByServicePort(&eps, svc), nil
}

func (s ServiceEndpoints) empty() bool {
	for _, endpoints := range s {
		if len(endpoints) > 0 {
			return false
		}
	}
	return true
}

func endpointsByServicePort(eps *corev1.Endpoints, svc *corev1.Service) ServiceEndpoints {
	if eps == nil || svc == nil {
		return nil
	}

	servicePorts := make(map[string]corev1.Protocol, len(svc.Spec.Ports))
	for _, p := range svc.Spec.Ports {
		protocol := p.Protocol
		if protocol == "" {
			protocol = corev1.ProtocolTCP
		}
		servicePorts[p.Name] = protocol
	}

	out := make(ServiceEndpoints)
	seen := make(map[string]map[string]struct{})
	for _, subset := range eps.Subsets {
		for _, port := range subset.Ports {
			protocol, ok := servicePorts[port.Name]
			if !ok {
				continue
			}
			if port.Protocol != "" && port.Protocol != protocol {
				continue
			}
			if seen[port.Name] == nil {
				seen[port.Name] = make(map[string]struct{})
			}
			for _, addr := range subset.Addresses {
				if addr.IP == "" {
					continue
				}
				upstream := net.JoinHostPort(addr.IP, fmt.Sprintf("%d", port.Port))
				if _, ok := seen[port.Name][upstream]; ok {
					continue
				}
				seen[port.Name][upstream] = struct{}{}
				out[port.Name] = append(out[port.Name], upstream)
			}
		}
	}
	for name := range out {
		sort.Strings(out[name])
	}
	return out
}

func servicePredicate() predicate.Predicate {
	match := func(obj client.Object) bool {
		svc, ok := obj.(*corev1.Service)
		if !ok {
			return false
		}
		return ServiceMatches(svc)
	}
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool { return match(e.Object) },
		UpdateFunc: func(e event.UpdateEvent) bool {
			// Reconcile when either side matches so type=LoadBalancer→ClusterIP
			// transitions trigger cleanup.
			return match(e.ObjectOld) || match(e.ObjectNew)
		},
		DeleteFunc:  func(e event.DeleteEvent) bool { return match(e.Object) },
		GenericFunc: func(e event.GenericEvent) bool { return match(e.Object) },
	}
}

func nodeToAllServices(r client.Reader) func(context.Context, client.Object) []ctrl.Request {
	return func(ctx context.Context, _ client.Object) []ctrl.Request {
		var svcs corev1.ServiceList
		if err := r.List(ctx, &svcs); err != nil {
			return nil
		}
		var reqs []ctrl.Request
		for _, svc := range svcs.Items {
			if ServiceMatches(&svc) {
				reqs = append(reqs, ctrl.Request{
					NamespacedName: types.NamespacedName{
						Namespace: svc.Namespace,
						Name:      svc.Name,
					},
				})
			}
		}
		return reqs
	}
}

func endpointToService(_ context.Context, obj client.Object) []ctrl.Request {
	if obj == nil {
		return nil
	}
	return []ctrl.Request{{
		NamespacedName: types.NamespacedName{
			Namespace: obj.GetNamespace(),
			Name:      obj.GetName(),
		},
	}}
}
