package managers

import (
	"context"
	"fmt"
	"sort"
	"testing"

	loxiapi "github.com/loxilb-io/kube-loxilb/pkg/api"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	netv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const testNs = "default"

func newTestReconciler(t *testing.T, objs ...client.Object) *LoxilbIngressReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to build scheme: %v", err)
	}
	return &LoxilbIngressReconciler{
		Client:     fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build(),
		Scheme:     scheme,
		LoxiClient: &loxiapi.LoxiClient{Host: "10.0.0.1"},
	}
}

func testService(name string, ports ...corev1.ServicePort) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNs, Name: name},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": name},
			Ports:    ports,
		},
	}
}

type slicePort struct {
	name     *string
	port     int32
	protocol corev1.Protocol
}

func testSlice(svcName, sliceName string, ports []slicePort, addrs ...string) *discoveryv1.EndpointSlice {
	slice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: testNs,
			Name:      sliceName,
			Labels:    map[string]string{discoveryv1.LabelServiceName: svcName},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
	}
	for _, p := range ports {
		port := p.port
		protocol := p.protocol
		if protocol == "" {
			protocol = corev1.ProtocolTCP
		}
		slice.Ports = append(slice.Ports, discoveryv1.EndpointPort{Name: p.name, Port: &port, Protocol: &protocol})
	}
	for _, addr := range addrs {
		slice.Endpoints = append(slice.Endpoints, discoveryv1.Endpoint{Addresses: []string{addr}})
	}
	return slice
}

func strPtr(s string) *string { return &s }

func byNumber(n int32) netv1.ServiceBackendPort { return netv1.ServiceBackendPort{Number: n} }
func byName(n string) netv1.ServiceBackendPort  { return netv1.ServiceBackendPort{Name: n} }

func epStrings(eps []loxiapi.LoadBalancerEndpoint) []string {
	out := make([]string, 0, len(eps))
	for _, ep := range eps {
		out = append(out, fmt.Sprintf("%s:%d", ep.EndpointIP, ep.TargetPort))
	}
	sort.Strings(out)
	return out
}

func assertEndpoints(t *testing.T, got []loxiapi.LoadBalancerEndpoint, want ...string) {
	t.Helper()
	sort.Strings(want)
	gotStr := epStrings(got)
	if len(gotStr) != len(want) {
		t.Fatalf("endpoints = %v, want %v", gotStr, want)
	}
	for i := range want {
		if gotStr[i] != want[i] {
			t.Fatalf("endpoints = %v, want %v", gotStr, want)
		}
	}
}

func TestCreateLoxiLoadBalancerEndpointsForBackend(t *testing.T) {
	tests := []struct {
		name        string
		objs        []client.Object
		backendPort netv1.ServiceBackendPort
		want        []string
		wantErr     bool
	}{
		{
			name: "port differs from targetPort, backend by number",
			objs: []client.Object{
				testService("web", corev1.ServicePort{Name: "http", Port: 80, TargetPort: intstr.FromInt32(8000)}),
				testSlice("web", "web-1", []slicePort{{name: strPtr("http"), port: 8000}}, "10.1.0.1", "10.1.0.2"),
			},
			backendPort: byNumber(80),
			want:        []string{"10.1.0.1:8000", "10.1.0.2:8000"},
		},
		{
			name: "backend by port name",
			objs: []client.Object{
				testService("web", corev1.ServicePort{Name: "http", Port: 80, TargetPort: intstr.FromInt32(8000)}),
				testSlice("web", "web-1", []slicePort{{name: strPtr("http"), port: 8000}}, "10.1.0.1"),
			},
			backendPort: byName("http"),
			want:        []string{"10.1.0.1:8000"},
		},
		{
			name: "multi-port service picks the referenced port only",
			objs: []client.Object{
				testService("web",
					corev1.ServicePort{Name: "http", Port: 80, TargetPort: intstr.FromInt32(8000)},
					corev1.ServicePort{Name: "https", Port: 443, TargetPort: intstr.FromInt32(8443)},
				),
				testSlice("web", "web-1", []slicePort{
					{name: strPtr("https"), port: 8443},
					{name: strPtr("http"), port: 8000},
				}, "10.1.0.1"),
			},
			backendPort: byNumber(443),
			want:        []string{"10.1.0.1:8443"},
		},
		{
			name: "named targetPort resolved per slice",
			objs: []client.Object{
				testService("web", corev1.ServicePort{Name: "http", Port: 80, TargetPort: intstr.FromString("web")}),
				testSlice("web", "web-old", []slicePort{{name: strPtr("http"), port: 8000}}, "10.1.0.1"),
				testSlice("web", "web-new", []slicePort{{name: strPtr("http"), port: 9000}}, "10.1.0.2"),
			},
			backendPort: byNumber(80),
			want:        []string{"10.1.0.1:8000", "10.1.0.2:9000"},
		},
		{
			name: "unnamed single-port service",
			objs: []client.Object{
				testService("web", corev1.ServicePort{Port: 80, TargetPort: intstr.FromInt32(8080)}),
				testSlice("web", "web-1", []slicePort{{name: strPtr(""), port: 8080}}, "10.1.0.1"),
			},
			backendPort: byNumber(80),
			want:        []string{"10.1.0.1:8080"},
		},
		{
			name: "unnamed single-port service with hand-written slice naming its only port",
			objs: []client.Object{
				testService("web", corev1.ServicePort{Port: 80}),
				testSlice("web", "web-manual", []slicePort{{name: strPtr("whatever"), port: 8080}}, "192.168.0.10"),
			},
			backendPort: byNumber(80),
			want:        []string{"192.168.0.10:8080"},
		},
		{
			name: "same port number on UDP and TCP uses the TCP port",
			objs: []client.Object{
				testService("dns",
					corev1.ServicePort{Name: "dns-udp", Port: 53, Protocol: corev1.ProtocolUDP, TargetPort: intstr.FromInt32(5353)},
					corev1.ServicePort{Name: "dns-tcp", Port: 53, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromInt32(5354)},
				),
				testSlice("dns", "dns-1", []slicePort{
					{name: strPtr("dns-udp"), port: 5353, protocol: corev1.ProtocolUDP},
					{name: strPtr("dns-tcp"), port: 5354},
				}, "10.1.0.1"),
			},
			backendPort: byNumber(53),
			want:        []string{"10.1.0.1:5354"},
		},
		{
			name: "backend port not defined on service",
			objs: []client.Object{
				testService("web", corev1.ServicePort{Name: "http", Port: 80, TargetPort: intstr.FromInt32(8000)}),
				testSlice("web", "web-1", []slicePort{{name: strPtr("http"), port: 8000}}, "10.1.0.1"),
			},
			backendPort: byNumber(8000),
			wantErr:     true,
		},
		{
			name: "backend port name not defined on service",
			objs: []client.Object{
				testService("web", corev1.ServicePort{Name: "http", Port: 80, TargetPort: intstr.FromInt32(8000)}),
				testSlice("web", "web-1", []slicePort{{name: strPtr("http"), port: 8000}}, "10.1.0.1"),
			},
			backendPort: byName("grpc"),
			wantErr:     true,
		},
		{
			name: "service not found",
			objs: []client.Object{
				testSlice("web", "web-1", []slicePort{{name: strPtr("http"), port: 8000}}, "10.1.0.1"),
			},
			backendPort: byNumber(80),
			wantErr:     true,
		},
		{
			name: "no slice publishes the referenced port",
			objs: []client.Object{
				testService("web",
					corev1.ServicePort{Name: "http", Port: 80, TargetPort: intstr.FromInt32(8000)},
					corev1.ServicePort{Name: "metrics", Port: 9090},
				),
				testSlice("web", "web-1", []slicePort{{name: strPtr("metrics"), port: 9090}}, "10.1.0.1"),
			},
			backendPort: byNumber(80),
			wantErr:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newTestReconciler(t, tt.objs...)
			backend := &netv1.IngressServiceBackend{Name: "web", Port: tt.backendPort}
			if svc, ok := tt.objs[0].(*corev1.Service); ok {
				backend.Name = svc.Name
			}

			got, err := r.createLoxiLoadBalancerEndpointsForBackend(context.Background(), testNs, backend)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got endpoints %v", epStrings(got))
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			assertEndpoints(t, got, tt.want...)
		})
	}
}

func TestCreateLoxiModelListUsesTargetPort(t *testing.T) {
	pathType := netv1.PathTypePrefix
	className := loxilbIngressClassName
	ingress := &netv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNs, Name: "web"},
		Spec: netv1.IngressSpec{
			IngressClassName: &className,
			Rules: []netv1.IngressRule{{
				Host: "www.example.com",
				IngressRuleValue: netv1.IngressRuleValue{HTTP: &netv1.HTTPIngressRuleValue{
					Paths: []netv1.HTTPIngressPath{{
						Path:     "/",
						PathType: &pathType,
						Backend: netv1.IngressBackend{Service: &netv1.IngressServiceBackend{
							Name: "web",
							Port: byNumber(80),
						}},
					}},
				}},
			}},
		},
	}

	r := newTestReconciler(t,
		testService("web", corev1.ServicePort{Name: "http", Port: 80, TargetPort: intstr.FromInt32(8000)}),
		testSlice("web", "web-1", []slicePort{{name: strPtr("http"), port: 8000}}, "10.1.0.1"),
	)

	models, skipped, err := r.createLoxiModelList(context.Background(), ingress)
	if err != nil || len(skipped) != 0 {
		t.Fatalf("unexpected error: %v, skipped: %v", err, skipped)
	}
	if len(models) != 1 {
		t.Fatalf("models = %d, want 1", len(models))
	}
	if models[0].Service.Port != 80 {
		t.Errorf("frontend port = %d, want 80", models[0].Service.Port)
	}
	assertEndpoints(t, models[0].Endpoints, "10.1.0.1:8000")
}
