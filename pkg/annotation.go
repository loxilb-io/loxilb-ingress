package pkg

const (
	// ExternalBackendServiceAnnotation marks an Ingress whose backend Services may be in other
	// namespaces. The namespace of a backend is in the annotation service-<name>-namespace.
	// kube-loxilb sets both on the Ingresses it makes from HTTPRoutes.
	ExternalBackendServiceAnnotation = "external-backend-service"
	DirectLBServiceAnnotation        = "loxilb.io/direct-loadbalance-service"
	DirectLBNamespaceAnnotation      = "loxilb.io/direct-loadbalance-namespace"
	EndPointSelAnnotation            = "loxilb.io/epselect"
	// TimeoutAnnotation is the inactivity timeout, in seconds, of an Ingress's HTTP and HTTPS
	// rules. kube-loxilb reads the same annotation on Services.
	TimeoutAnnotation = "loxilb.io/timeout"
)

const (
	// endPointSelAnnotation is the annotation key for the endpoint selection
	EndPointSel_RR       = "rr"
	EndPointSel_HASH     = "hash"
	EndpointSel_PRIORITY = "priority"
	EndPointSel_PERSIST  = "persist"
	EndPointSel_LC       = "lc"
	EndPointSel_N2       = "n2"
)
