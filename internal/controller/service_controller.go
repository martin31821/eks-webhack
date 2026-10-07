package controller

import (
	"context"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// ServiceReconciler watches Services and creates NodePort clones for webhook-backing ClusterIP services.
// It patches the original service's selector to exclude all pods so that the built-in
// EndpointSlice controller produces an empty slice, leaving the field clear for our translated slices.
//
// The selector is only poisoned once EndpointProxyReconciler has published a managed EndpointSlice,
// so there is never a window where the webhook has zero reachable endpoints.
type ServiceReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

func (r *ServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	svc := &corev1.Service{}
	if err := r.Get(ctx, req.NamespacedName, svc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Never process clones we own — their selector must stay as-is.
	if svc.Labels[LabelManagedBy] == LabelManagedByValue {
		return ctrl.Result{}, nil
	}

	if !svc.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, svc)
	}

	refs, err := getWebhookServiceRefs(ctx, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}

	isBacking := isWebhookBackingService(refs, svc.Namespace, svc.Name)
	isManageable := svc.Spec.Type == corev1.ServiceTypeClusterIP && svc.Spec.ClusterIP != "None"

	if !isBacking || !isManageable {
		if controllerutil.ContainsFinalizer(svc, FinalizerName) {
			logger.Info("service no longer needs proxy, cleaning up", "service", req.NamespacedName)
			return r.cleanupAndRemoveFinalizer(ctx, svc)
		}
		return ctrl.Result{}, nil
	}

	// Step 1: Add finalizer — safe to do before proxy coverage exists.
	if !controllerutil.ContainsFinalizer(svc, FinalizerName) {
		patch := client.MergeFrom(svc.DeepCopy())
		controllerutil.AddFinalizer(svc, FinalizerName)
		if err := r.Patch(ctx, svc, patch); err != nil {
			return ctrl.Result{}, err
		}
		// svc is updated in-place with the new resourceVersion by the Patch call.
	}

	// Step 2: Ensure the NodePort clone exists and is up to date.
	if err := r.ensureNodePortClone(ctx, svc); err != nil {
		return ctrl.Result{}, err
	}

	// Step 3: Only poison the original Service's selector once EndpointProxyReconciler has
	// published a managed EndpointSlice. This guarantees node-IP endpoints are live before
	// we cut over, eliminating any window with zero reachable webhook endpoints.
	if svc.Spec.Selector[LabelExcludeFromEndpointsKey] == LabelExcludeFromEndpointsValue {
		return ctrl.Result{}, nil // already done
	}

	ready, err := r.managedEPSExists(ctx, svc)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !ready {
		logger.Info("waiting for managed EndpointSlice before patching service selector",
			"service", req.NamespacedName)
		// Re-triggered by the Watch on managed EndpointSlices when the EPS is created.
		return ctrl.Result{}, nil
	}

	base := svc.DeepCopy()
	if svc.Spec.Selector == nil {
		svc.Spec.Selector = map[string]string{}
	}
	svc.Spec.Selector[LabelExcludeFromEndpointsKey] = LabelExcludeFromEndpointsValue
	logger.Info("injecting exclude-from-endpoints label into selector", "service", req.NamespacedName)
	return ctrl.Result{}, r.Patch(ctx, svc, client.MergeFrom(base))
}

func (r *ServiceReconciler) handleDeletion(ctx context.Context, svc *corev1.Service) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(svc, FinalizerName) {
		return ctrl.Result{}, nil
	}
	patch := client.MergeFrom(svc.DeepCopy())
	controllerutil.RemoveFinalizer(svc, FinalizerName)
	return ctrl.Result{}, r.Patch(ctx, svc, patch)
}

func (r *ServiceReconciler) cleanupAndRemoveFinalizer(ctx context.Context, svc *corev1.Service) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Explicitly delete the clone so the EndpointProxyReconciler can clean up managed
	// EndpointSlices before the selector is restored.
	cloneName := CloneServiceName(svc.Name)
	clone := &corev1.Service{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: svc.Namespace, Name: cloneName}, clone); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	} else if clone.Labels[LabelManagedBy] == LabelManagedByValue {
		logger.Info("deleting NodePort clone", "clone", cloneName)
		if err := r.Delete(ctx, clone); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}

	// Restore the original selector if it was patched.
	if _, ok := svc.Spec.Selector[LabelExcludeFromEndpointsKey]; ok {
		base := svc.DeepCopy()
		delete(svc.Spec.Selector, LabelExcludeFromEndpointsKey)
		logger.Info("restoring original service selector", "service", svc.Name)
		if err := r.Patch(ctx, svc, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, err
		}
		// Re-fetch so resourceVersion is current for the finalizer patch below.
		if err := r.Get(ctx, types.NamespacedName{Namespace: svc.Namespace, Name: svc.Name}, svc); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
	}

	patch := client.MergeFrom(svc.DeepCopy())
	controllerutil.RemoveFinalizer(svc, FinalizerName)
	return ctrl.Result{}, r.Patch(ctx, svc, patch)
}

// managedEPSExists returns true when at least one EndpointSlice managed by this controller
// exists for svc. Presence (even with zero endpoints) proves EndpointProxyReconciler has run
// at least once and proxy infrastructure is in place.
func (r *ServiceReconciler) managedEPSExists(ctx context.Context, svc *corev1.Service) (bool, error) {
	var list discoveryv1.EndpointSliceList
	if err := r.List(ctx, &list,
		client.InNamespace(svc.Namespace),
		client.MatchingLabels{
			discoveryv1.LabelServiceName: svc.Name,
			LabelManagedBy:               LabelManagedByValue,
		},
	); err != nil {
		return false, err
	}
	return len(list.Items) > 0, nil
}

func (r *ServiceReconciler) ensureNodePortClone(ctx context.Context, svc *corev1.Service) error {
	cloneName := CloneServiceName(svc.Name)

	existing := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Namespace: svc.Namespace, Name: cloneName}, existing)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}

	desired := buildNodePortClone(svc, cloneName)
	if err := ctrl.SetControllerReference(svc, desired, r.Scheme); err != nil {
		return err
	}

	if apierrors.IsNotFound(err) {
		log.FromContext(ctx).Info("creating NodePort clone", "clone", cloneName, "original", svc.Name)
		return r.Create(ctx, desired)
	}

	// Keep clone selector and ports in sync with the original.
	patch := client.MergeFrom(existing.DeepCopy())
	existing.Spec.Selector = desired.Spec.Selector
	existing.Spec.Ports = desired.Spec.Ports
	return r.Patch(ctx, existing, patch)
}

func buildNodePortClone(svc *corev1.Service, name string) *corev1.Service {
	selector := make(map[string]string, len(svc.Spec.Selector))
	for k, v := range svc.Spec.Selector {
		if k != LabelExcludeFromEndpointsKey {
			selector[k] = v
		}
	}

	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: svc.Namespace,
			Labels: map[string]string{
				LabelManagedBy:       LabelManagedByValue,
				LabelOriginalService: svc.Name,
			},
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeNodePort,
			Selector: selector,
			Ports:    clonePortsForNodePort(svc.Spec.Ports),
		},
	}
}

// clonePortsForNodePort copies service ports without pinning a NodePort value so
// Kubernetes can assign one dynamically.
func clonePortsForNodePort(ports []corev1.ServicePort) []corev1.ServicePort {
	out := make([]corev1.ServicePort, len(ports))
	for i, p := range ports {
		out[i] = corev1.ServicePort{
			Name:       p.Name,
			Protocol:   p.Protocol,
			Port:       p.Port,
			TargetPort: p.TargetPort,
		}
	}
	return out
}

func (r *ServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("service-controller").
		For(&corev1.Service{}).
		Watches(
			&admissionv1.ValidatingWebhookConfiguration{},
			handler.EnqueueRequestsFromMapFunc(mapValidatingWebhookToServices),
		).
		Watches(
			&admissionv1.MutatingWebhookConfiguration{},
			handler.EnqueueRequestsFromMapFunc(mapMutatingWebhookToServices),
		).
		Watches(
			&apiextensionsv1.CustomResourceDefinition{},
			handler.EnqueueRequestsFromMapFunc(mapCRDToServices),
		).
		// When EndpointProxyReconciler publishes a managed EndpointSlice, re-queue the
		// original Service so we can proceed to patch its selector (step 3 above).
		Watches(
			&discoveryv1.EndpointSlice{},
			handler.EnqueueRequestsFromMapFunc(mapManagedEPSToService),
		).
		Owns(&corev1.Service{}).
		Complete(r)
}
