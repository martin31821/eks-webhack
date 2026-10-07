package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// EndpointProxyReconciler watches NodePort clone Services (and their EndpointSlices) and
// maintains translated EndpointSlices on the original Service that contain Node IPs and
// NodePorts instead of Pod IPs and container ports.
//
// The EKS control plane can reach Node IPs via VPC routing; kube-proxy on each node then
// forwards NodePort traffic to the pod through the overlay network.
type EndpointProxyReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// Reconcile is triggered with the NamespacedName of a NodePort clone Service.
func (r *EndpointProxyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	clone := &corev1.Service{}
	if err := r.Get(ctx, req.NamespacedName, clone); err != nil {
		if apierrors.IsNotFound(err) {
			return r.cleanupManagedEPS(ctx, req.NamespacedName)
		}
		return ctrl.Result{}, err
	}

	if clone.Labels[LabelManagedBy] != LabelManagedByValue {
		return ctrl.Result{}, nil
	}

	originalSvcName := clone.Labels[LabelOriginalService]
	if originalSvcName == "" {
		return ctrl.Result{}, nil
	}

	// List all EndpointSlices belonging to this NodePort clone.
	var epsList discoveryv1.EndpointSliceList
	if err := r.List(ctx, &epsList,
		client.InNamespace(clone.Namespace),
		client.MatchingLabels{discoveryv1.LabelServiceName: clone.Name},
	); err != nil {
		return ctrl.Result{}, err
	}

	desiredNames := make(map[string]struct{})

	for i := range epsList.Items {
		src := &epsList.Items[i]
		// Skip slices we created to avoid a reconcile loop.
		if src.Labels[discoveryv1.LabelManagedBy] == LabelEPSManagedByValue {
			continue
		}

		managedName := ManagedEndpointSliceName(src.Name)
		desiredNames[managedName] = struct{}{}

		translated, err := r.translateEndpointSlice(ctx, clone, originalSvcName, src, managedName)
		if err != nil {
			logger.Error(err, "failed to translate EndpointSlice", "source", src.Name)
			return ctrl.Result{}, err
		}

		if err := r.applyManagedEPS(ctx, translated); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Remove any stale managed slices that no longer have a source.
	var existing discoveryv1.EndpointSliceList
	if err := r.List(ctx, &existing,
		client.InNamespace(clone.Namespace),
		client.MatchingLabels{
			discoveryv1.LabelManagedBy: LabelManagedByValue,
			LabelSourceService:         clone.Name,
		},
	); err != nil {
		return ctrl.Result{}, err
	}
	for i := range existing.Items {
		eps := &existing.Items[i]
		if _, keep := desiredNames[eps.Name]; !keep {
			logger.Info("deleting stale managed EndpointSlice", "name", eps.Name)
			if err := r.Delete(ctx, eps); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
		}
	}

	return ctrl.Result{}, nil
}

func (r *EndpointProxyReconciler) translateEndpointSlice(
	ctx context.Context,
	clone *corev1.Service,
	originalSvcName string,
	src *discoveryv1.EndpointSlice,
	managedName string,
) (*discoveryv1.EndpointSlice, error) {
	ports := translateToNodePorts(clone.Spec.Ports, src.Ports)

	var endpoints []discoveryv1.Endpoint
	for _, ep := range src.Endpoints {
		if ep.NodeName == nil {
			continue
		}
		node := &corev1.Node{}
		if err := r.Get(ctx, types.NamespacedName{Name: *ep.NodeName}, node); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, err
		}
		nodeIP := nodeInternalIP(node)
		if nodeIP == "" {
			continue
		}
		endpoints = append(endpoints, discoveryv1.Endpoint{
			Addresses:  []string{nodeIP},
			Conditions: ep.Conditions,
			NodeName:   ep.NodeName,
		})
	}

	endpoints = deduplicateEndpoints(endpoints)

	return &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      managedName,
			Namespace: clone.Namespace,
			Labels: map[string]string{
				discoveryv1.LabelServiceName: originalSvcName,
				discoveryv1.LabelManagedBy:   LabelEPSManagedByValue,
				LabelManagedBy:               LabelManagedByValue,
				LabelSourceService:           clone.Name,
			},
		},
		AddressType: src.AddressType,
		Endpoints:   endpoints,
		Ports:       ports,
	}, nil
}

func (r *EndpointProxyReconciler) applyManagedEPS(ctx context.Context, desired *discoveryv1.EndpointSlice) error {
	existing := &discoveryv1.EndpointSlice{}
	err := r.Get(ctx, types.NamespacedName{Namespace: desired.Namespace, Name: desired.Name}, existing)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return r.Create(ctx, desired)
		}
		return err
	}
	patch := client.MergeFrom(existing.DeepCopy())
	existing.AddressType = desired.AddressType
	existing.Endpoints = desired.Endpoints
	existing.Ports = desired.Ports
	existing.Labels = desired.Labels
	return r.Patch(ctx, existing, patch)
}

func (r *EndpointProxyReconciler) cleanupManagedEPS(ctx context.Context, cloneKey types.NamespacedName) (ctrl.Result, error) {
	var list discoveryv1.EndpointSliceList
	if err := r.List(ctx, &list,
		client.InNamespace(cloneKey.Namespace),
		client.MatchingLabels{
			LabelManagedBy:     LabelManagedByValue,
			LabelSourceService: cloneKey.Name,
		},
	); err != nil {
		return ctrl.Result{}, err
	}
	for i := range list.Items {
		eps := &list.Items[i]
		if err := r.Delete(ctx, eps); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

// translateToNodePorts maps EndpointSlice ports (container/target ports) to the corresponding
// NodePort values assigned by Kubernetes on the clone Service.
func translateToNodePorts(svcPorts []corev1.ServicePort, epsPorts []discoveryv1.EndpointPort) []discoveryv1.EndpointPort {
	var out []discoveryv1.EndpointPort
	for _, ep := range epsPorts {
		np := findNodePort(svcPorts, ep)
		if np == 0 {
			continue
		}
		port := np
		out = append(out, discoveryv1.EndpointPort{
			Name:     ep.Name,
			Protocol: ep.Protocol,
			Port:     &port,
		})
	}
	return out
}

func findNodePort(svcPorts []corev1.ServicePort, ep discoveryv1.EndpointPort) int32 {
	// Primary: match by port name.
	if ep.Name != nil && *ep.Name != "" {
		for _, sp := range svcPorts {
			if sp.Name == *ep.Name {
				return sp.NodePort
			}
		}
	}
	// Secondary: match by numeric target port.
	if ep.Port != nil {
		for _, sp := range svcPorts {
			if sp.TargetPort.Type == intstr.Int && sp.TargetPort.IntVal == *ep.Port {
				return sp.NodePort
			}
		}
	}
	// Fallback for single-port services.
	if len(svcPorts) == 1 {
		return svcPorts[0].NodePort
	}
	return 0
}

func nodeInternalIP(node *corev1.Node) string {
	for _, addr := range node.Status.Addresses {
		if addr.Type == corev1.NodeInternalIP {
			return addr.Address
		}
	}
	for _, addr := range node.Status.Addresses {
		if addr.Type == corev1.NodeExternalIP {
			return addr.Address
		}
	}
	return ""
}

// deduplicateEndpoints merges endpoints that share the same node IP.
// When multiple pods on a node contribute endpoints, the node IP appears once
// and is marked ready if any of the source endpoints is ready.
func deduplicateEndpoints(endpoints []discoveryv1.Endpoint) []discoveryv1.Endpoint {
	seen := make(map[string]int)
	var out []discoveryv1.Endpoint
	for _, ep := range endpoints {
		if len(ep.Addresses) == 0 {
			continue
		}
		addr := ep.Addresses[0]
		if idx, exists := seen[addr]; exists {
			if ep.Conditions.Ready != nil && *ep.Conditions.Ready {
				ready := true
				out[idx].Conditions.Ready = &ready
			}
		} else {
			seen[addr] = len(out)
			out = append(out, ep)
		}
	}
	return out
}

func (r *EndpointProxyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("endpoint-proxy-controller").
		// Primary resource: NodePort clone Services.
		For(&corev1.Service{}, builder.WithPredicates(
			predicate.NewPredicateFuncs(func(obj client.Object) bool {
				return obj.GetLabels()[LabelManagedBy] == LabelManagedByValue
			}),
		)).
		// Secondary resource: EndpointSlices — map to the owning Service so we re-reconcile
		// the clone whenever its EndpointSlice changes.
		Watches(
			&discoveryv1.EndpointSlice{},
			handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj client.Object) []reconcile.Request {
				svcName := obj.GetLabels()[discoveryv1.LabelServiceName]
				if svcName == "" {
					return nil
				}
				return []reconcile.Request{{
					NamespacedName: types.NamespacedName{
						Namespace: obj.GetNamespace(),
						Name:      svcName,
					},
				}}
			}),
			// Skip EndpointSlices we manage to avoid a reconcile loop.
			builder.WithPredicates(predicate.NewPredicateFuncs(func(obj client.Object) bool {
				return obj.GetLabels()[discoveryv1.LabelManagedBy] != LabelEPSManagedByValue
			})),
		).
		Complete(r)
}
