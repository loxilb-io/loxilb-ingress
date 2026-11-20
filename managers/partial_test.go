package managers

import (
	"context"
	"sort"
	"testing"

	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// twoPathIngress routes /a to the web Service and /b to the api Service.
func twoPathIngress() *netv1.Ingress {
	ing := testIngress("/a", 1)
	paths := &ing.Spec.Rules[0].HTTP.Paths
	b := (*paths)[0]
	b.Path = "/b"
	b.Backend = netv1.IngressBackend{Service: &netv1.IngressServiceBackend{Name: "api", Port: byNumber(80)}}
	*paths = append(*paths, b)
	return ing
}

func sortedPaths(lb *fakeLoxilb) []string {
	paths := lb.paths()
	sort.Strings(paths)
	return paths
}

func assertPaths(t *testing.T, lb *fakeLoxilb, want ...string) {
	t.Helper()
	got := sortedPaths(lb)
	if len(got) != len(want) {
		t.Fatalf("loxilb paths = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("loxilb paths = %v, want %v", got, want)
		}
	}
}

func TestFailingBackendLeavesOtherPathsInstalled(t *testing.T) {
	ctx := context.Background()
	pods := newTestPods(t, []string{"loxilb-ingress-aaaaa"}, append(webBackend(), twoPathIngress())...)
	a := pods[0]
	c := a.r.Client

	// The api Service doesn't exist yet: /a is installed and reported, and the Ingress is retried.
	assertRetrying(t, a.r, ctx)
	assertPaths(t, a.loxilb, "/a")
	assertMark(t, getIngress(t, c), a.markKey, "1")

	// api gets endpoints: both paths are installed and the normal resync interval is back.
	apiSlice := testSlice("api", "api-1", []slicePort{{name: strPtr("http"), port: 9000}}, "10.2.0.1")
	if err := c.Create(ctx, testService("api", corev1.ServicePort{Name: "http", Port: 80, TargetPort: intstr.FromInt32(9000)})); err != nil {
		t.Fatal(err)
	}
	if err := c.Create(ctx, apiSlice); err != nil {
		t.Fatal(err)
	}
	res, err := a.r.Reconcile(ctx, webReq)
	if err != nil {
		t.Fatal(err)
	}
	if res.RequeueAfter == backendRetryInterval {
		t.Fatalf("RequeueAfter = %v, want the normal resync interval", res.RequeueAfter)
	}
	assertPaths(t, a.loxilb, "/a", "/b")

	// api loses its endpoints: only /b goes away, and the report stays.
	if err := c.Delete(ctx, apiSlice); err != nil {
		t.Fatal(err)
	}
	assertRetrying(t, a.r, ctx)
	assertPaths(t, a.loxilb, "/a")
	assertMark(t, getIngress(t, c), a.markKey, "1")
}

func TestAllBackendsFailingRemovesRulesAndReport(t *testing.T) {
	ctx := context.Background()
	webSlice := testSlice("web", "web-1", []slicePort{{name: strPtr("http"), port: 8000}}, "10.1.0.1")
	pods := newTestPods(t, []string{"loxilb-ingress-aaaaa"},
		testService("web", corev1.ServicePort{Name: "http", Port: 80, TargetPort: intstr.FromInt32(8000)}),
		webSlice,
		twoPathIngress(),
	)
	a := pods[0]
	c := a.r.Client

	assertRetrying(t, a.r, ctx)
	assertPaths(t, a.loxilb, "/a")

	if err := c.Delete(ctx, webSlice); err != nil {
		t.Fatal(err)
	}
	assertRetrying(t, a.r, ctx)
	assertPaths(t, a.loxilb)
	assertMark(t, getIngress(t, c), a.markKey, "")
}

func TestDirectLoadBalanceSkipsPortsWithoutEndpoints(t *testing.T) {
	ingress := testIngress("/", 1)
	ingress.Annotations = map[string]string{"loxilb.io/direct-loadbalance-service": "web"}

	r := newTestReconciler(t,
		testService("web",
			corev1.ServicePort{Name: "http", Port: 80, TargetPort: intstr.FromInt32(8000)},
			corev1.ServicePort{Name: "metrics", Port: 9090},
		),
		testSlice("web", "web-1", []slicePort{{name: strPtr("http"), port: 8000}}, "10.1.0.1"),
	)

	models, skipped, err := r.createDirectLoxiModelList(context.Background(), ingress)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].Service.Port != 80 {
		t.Fatalf("models = %+v, want only port 80", models)
	}
	if len(skipped) != 1 {
		t.Fatalf("skipped = %v, want the metrics port", skipped)
	}
}
