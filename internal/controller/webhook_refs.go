package controller

import (
	"context"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type serviceRef struct {
	Namespace string
	Name      string
}

// getWebhookServiceRefs returns all Service references found in ValidatingWebhookConfigurations,
// MutatingWebhookConfigurations, and CRD conversion webhooks.
func getWebhookServiceRefs(ctx context.Context, c client.Client) ([]serviceRef, error) {
	var refs []serviceRef

	var vwhList admissionv1.ValidatingWebhookConfigurationList
	if err := c.List(ctx, &vwhList); err != nil {
		return nil, err
	}
	for _, vwh := range vwhList.Items {
		for _, hook := range vwh.Webhooks {
			if svc := hook.ClientConfig.Service; svc != nil {
				refs = append(refs, serviceRef{Namespace: svc.Namespace, Name: svc.Name})
			}
		}
	}

	var mwhList admissionv1.MutatingWebhookConfigurationList
	if err := c.List(ctx, &mwhList); err != nil {
		return nil, err
	}
	for _, mwh := range mwhList.Items {
		for _, hook := range mwh.Webhooks {
			if svc := hook.ClientConfig.Service; svc != nil {
				refs = append(refs, serviceRef{Namespace: svc.Namespace, Name: svc.Name})
			}
		}
	}

	var crdList apiextensionsv1.CustomResourceDefinitionList
	if err := c.List(ctx, &crdList); err != nil {
		return nil, err
	}
	for _, crd := range crdList.Items {
		if crd.Spec.Conversion == nil ||
			crd.Spec.Conversion.Webhook == nil ||
			crd.Spec.Conversion.Webhook.ClientConfig == nil {
			continue
		}
		if svc := crd.Spec.Conversion.Webhook.ClientConfig.Service; svc != nil {
			refs = append(refs, serviceRef{Namespace: svc.Namespace, Name: svc.Name})
		}
	}

	return refs, nil
}

func isWebhookBackingService(refs []serviceRef, ns, name string) bool {
	for _, r := range refs {
		if r.Namespace == ns && r.Name == name {
			return true
		}
	}
	return false
}

func mapValidatingWebhookToServices(_ context.Context, obj client.Object) []reconcile.Request {
	vwh, ok := obj.(*admissionv1.ValidatingWebhookConfiguration)
	if !ok {
		return nil
	}
	return refsFromVWH(vwh)
}

func mapMutatingWebhookToServices(_ context.Context, obj client.Object) []reconcile.Request {
	mwh, ok := obj.(*admissionv1.MutatingWebhookConfiguration)
	if !ok {
		return nil
	}
	return refsFromMWH(mwh)
}

func mapCRDToServices(_ context.Context, obj client.Object) []reconcile.Request {
	crd, ok := obj.(*apiextensionsv1.CustomResourceDefinition)
	if !ok {
		return nil
	}
	if crd.Spec.Conversion == nil ||
		crd.Spec.Conversion.Webhook == nil ||
		crd.Spec.Conversion.Webhook.ClientConfig == nil {
		return nil
	}
	svc := crd.Spec.Conversion.Webhook.ClientConfig.Service
	if svc == nil {
		return nil
	}
	return []reconcile.Request{{
		NamespacedName: types.NamespacedName{Namespace: svc.Namespace, Name: svc.Name},
	}}
}

func refsFromVWH(vwh *admissionv1.ValidatingWebhookConfiguration) []reconcile.Request {
	seen := make(map[types.NamespacedName]struct{})
	var out []reconcile.Request
	for _, hook := range vwh.Webhooks {
		if svc := hook.ClientConfig.Service; svc != nil {
			nn := types.NamespacedName{Namespace: svc.Namespace, Name: svc.Name}
			if _, exists := seen[nn]; !exists {
				seen[nn] = struct{}{}
				out = append(out, reconcile.Request{NamespacedName: nn})
			}
		}
	}
	return out
}

// mapManagedEPSToService re-queues the original Service when one of our managed
// EndpointSlices is created or updated, so ServiceReconciler can proceed with
// the selector patch once proxy coverage is confirmed (step 3).
func mapManagedEPSToService(_ context.Context, obj client.Object) []reconcile.Request {
	if obj.GetLabels()[LabelManagedBy] != LabelManagedByValue {
		return nil
	}
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
}

func refsFromMWH(mwh *admissionv1.MutatingWebhookConfiguration) []reconcile.Request {
	seen := make(map[types.NamespacedName]struct{})
	var out []reconcile.Request
	for _, hook := range mwh.Webhooks {
		if svc := hook.ClientConfig.Service; svc != nil {
			nn := types.NamespacedName{Namespace: svc.Namespace, Name: svc.Name}
			if _, exists := seen[nn]; !exists {
				seen[nn] = struct{}{}
				out = append(out, reconcile.Request{NamespacedName: nn})
			}
		}
	}
	return out
}
