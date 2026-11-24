package managers

import (
	"context"
	"sort"
	"testing"

	netv1 "k8s.io/api/networking/v1"

	"loxilb.io/loxilb-ingress-manager/pkg"
)

func namedIngress(name, backend string) *netv1.Ingress {
	ing := testIngress("/", 1)
	ing.Name = name
	ing.Spec.Rules[0].HTTP.Paths[0].Backend.Service.Name = backend
	return ing
}

func TestIngressesForEndpointSlice(t *testing.T) {
	sameNs := namedIngress("same-ns", "web")

	otherNs := namedIngress("other-ns", "api")
	otherNs.Annotations = map[string]string{
		pkg.ExternalBackendServiceAnnotation: "true",
		"service-api-namespace":              "backends",
	}

	direct := namedIngress("direct", "ignored")
	direct.Annotations = map[string]string{
		pkg.DirectLBServiceAnnotation:   "tcp",
		pkg.DirectLBNamespaceAnnotation: "backends",
	}

	notLoxilb := namedIngress("not-loxilb", "web")
	otherClass := "nginx"
	notLoxilb.Spec.IngressClassName = &otherClass

	r := newTestReconciler(t, sameNs, otherNs, direct, notLoxilb)

	tests := []struct {
		name      string
		namespace string
		service   string
		want      []string
	}{
		{"backend in the Ingress's namespace", testNs, "web", []string{"same-ns"}},
		{"backend in another namespace", "backends", "api", []string{"other-ns"}},
		{"same Service name in the Ingress's namespace is not the backend", testNs, "api", nil},
		{"direct-loadbalance Service", "backends", "tcp", []string{"direct"}},
		{"rules of a direct-loadbalance Ingress are not backends", testNs, "ignored", nil},
		{"Service no Ingress uses", testNs, "unused", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			epSlice := testSlice(tt.service, tt.service+"-1", []slicePort{{name: strPtr("http"), port: 8000}}, "10.1.0.1")
			epSlice.Namespace = tt.namespace

			got := []string{}
			for _, req := range r.ingressesForEndpointSlice(context.Background(), epSlice) {
				got = append(got, req.Name)
			}
			sort.Strings(got)
			if len(got) != len(tt.want) {
				t.Fatalf("Ingresses = %v, want %v", got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Fatalf("Ingresses = %v, want %v", got, tt.want)
				}
			}
		})
	}

	// A slice without the Service label belongs to no Service.
	orphan := testSlice("web", "orphan", nil, "10.1.0.1")
	orphan.Labels = nil
	if got := r.ingressesForEndpointSlice(context.Background(), orphan); len(got) != 0 {
		t.Fatalf("Ingresses for a slice without a Service = %v, want none", got)
	}
}
