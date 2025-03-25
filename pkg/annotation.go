package pkg

const (
	// AnnotationKey is the key for the annotation
	ExternalBackendServiceAnnotation = "external-backend-service" // Deprecated
	DirectLBServiceAnnotation        = "loxilb.io/direct-loadbalance-service"
	DirectLBNamespaceAnnotation      = "loxilb.io/direct-loadbalance-namespace"
	EndPointSelAnnotation            = "loxilb.io/epselect"
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
