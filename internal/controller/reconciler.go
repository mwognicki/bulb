package controller

import (
	"context"
	"fmt"
	"sort"

	bulbv1alpha1 "github.com/mwognicki/bulb/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

	Namespace        string
	Image            string
	NodeIPsConfigMap string
}

// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=services/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core,resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch
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

	desired, err := BuildDaemonSet(&svc, r.Image, r.Namespace)
	if err != nil {
		logger.Error(err, "build daemonset failed")
		return ctrl.Result{}, err
	}

	if err := r.applyDaemonSet(ctx, desired); err != nil {
		return ctrl.Result{}, err
	}
	lbports, err := r.BuildLBPorts(ctx, &svc)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("build lbports: %w", err)
	}
	if err := r.applyLBPorts(ctx, lbports, &svc); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.pruneStaleAppliedNodes(ctx, &svc); err != nil {
		logger.Error(err, "prune stale applied nodes")
	}
	r.emitLBPortEvents(ctx, &svc)

	ips, err := r.publicIPs(ctx)
	if err != nil {
		logger.Error(err, "read node-ips configmap; status will not be updated this round")
		return ctrl.Result{}, err
	}

	dnsRec := BuildDNSRecord(&svc, ips)
	if err := r.applyDNSRecord(ctx, dnsRec, &svc); err != nil {
		return ctrl.Result{}, fmt.Errorf("apply dnsrecord: %w", err)
	}
	logDNSConfig(ctx, &svc, dnsRec)

	if err := r.updateStatus(ctx, &svc, ips); err != nil {
		return ctrl.Result{}, fmt.Errorf("update service status: %w", err)
	}

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

// publicIPs reads the node-ips ConfigMap and returns a sorted list of
// public IPv4 addresses, one per node entry.
//
// Phase 4 will replace this with a node-annotation read (the
// node-ip-labeler DaemonSet will populate them).
func (r *ServiceReconciler) publicIPs(ctx context.Context) ([]string, error) {
	if r.NodeIPsConfigMap == "" {
		return nil, nil
	}
	var cm corev1.ConfigMap
	key := types.NamespacedName{Namespace: r.Namespace, Name: r.NodeIPsConfigMap}
	if err := r.Get(ctx, key, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("get node-ips configmap: %w", err)
	}
	ips := make([]string, 0, len(cm.Data))
	for _, v := range cm.Data {
		if v != "" {
			ips = append(ips, v)
		}
	}
	sort.Strings(ips)
	return ips, nil
}

func (r *ServiceReconciler) updateStatus(ctx context.Context, svc *corev1.Service, ips []string) error {
	desired := make([]corev1.LoadBalancerIngress, 0, len(ips))
	for _, ip := range ips {
		desired = append(desired, corev1.LoadBalancerIngress{IP: ip})
	}
	if equality.Semantic.DeepEqual(svc.Status.LoadBalancer.Ingress, desired) {
		return nil
	}
	patched := svc.DeepCopy()
	patched.Status.LoadBalancer.Ingress = desired
	return r.Status().Patch(ctx, patched, client.MergeFrom(svc))
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
		Complete(r)
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
