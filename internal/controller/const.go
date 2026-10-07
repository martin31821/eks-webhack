package controller

const (
	// LabelManagedBy is set on NodePort clone Services and managed EndpointSlices.
	LabelManagedBy      = "vlair.io/managed-by"
	LabelManagedByValue = "eks-webhack"

	// LabelOriginalService is set on the NodePort clone to record the original Service name.
	LabelOriginalService = "vlair.io/original-service"

	// LabelSourceService is set on managed EndpointSlices to record the NodePort clone name.
	LabelSourceService = "vlair.io/source-service"

	// LabelExcludeFromEndpointsKey/Value are injected into the original Service's selector so
	// the built-in EndpointSlice controller produces an empty slice for it.
	LabelExcludeFromEndpointsKey   = "vlair.io/exclude-from-endpoints"
	LabelExcludeFromEndpointsValue = "true"

	// LabelEPSManagedByValue is the value we use for the standard endpointslice managed-by label.
	// Setting this to a non-standard value causes the built-in EndpointSlice controller to ignore
	// our slices while still making them visible to kube-proxy and the API server.
	LabelEPSManagedByValue = "eks-webhack"

	// FinalizerName is placed on original Services while we are managing them so we can
	// restore their selector before they are removed from our watch.
	FinalizerName = "vlair.io/webhook-proxy"
)
