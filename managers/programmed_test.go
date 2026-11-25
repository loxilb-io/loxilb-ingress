package managers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	loxiapi "github.com/loxilb-io/kube-loxilb/pkg/api"
	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// fakeLoxilb serves the part of the loxilb REST API that the reconciler uses.
type fakeLoxilb struct {
	mu    sync.Mutex
	rules []loxiapi.LoadBalancerModel
	srv   *httptest.Server
	// failing makes every request fail, as a loxilb that isn't serving its API yet.
	failing bool
	// ruleCreates counts the rules created so far.
	ruleCreates int

	// certDir is where SNI certificates are read from when they are registered.
	certDir string
	// sni holds, per registered host, the certificate file content read at registration.
	// Like loxilb, it doesn't read the file again afterwards.
	sni map[string]string
	// sniRegistrations counts the certificates loaded so far.
	sniRegistrations int
}

func newFakeLoxilb(t *testing.T) *fakeLoxilb {
	t.Helper()
	f := &fakeLoxilb{}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeLoxilb) serve(w http.ResponseWriter, req *http.Request) {
	const base = "/netlox/v1/config/loadbalancer"
	const sniBase = "/netlox/v1/sni/certificates"
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failing {
		http.Error(w, "loxilb is not ready", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")

	switch {
	case req.URL.Path == sniBase && (req.Method == http.MethodPost || req.Method == http.MethodDelete):
		// loxilb answers these with 200 and tells the outcome in the result.
		_ = json.NewEncoder(w).Encode(map[string]string{"result": f.serveSNI(req)})
		return
	case req.Method == http.MethodGet && req.URL.Path == base+"/all":
		_ = json.NewEncoder(w).Encode(loxiapi.LoadBalancerListModel{Item: f.rules})
		return
	case req.Method == http.MethodPost && req.URL.Path == base:
		var model loxiapi.LoadBalancerModel
		if err := json.NewDecoder(req.Body).Decode(&model); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// loxilb has one rule per address, port, protocol, host, path and path type. A rule
		// posted for one that exists replaces it, whatever its name.
		replaced := false
		for i := range f.rules {
			if sameRuleKey(f.rules[i].Service, model.Service) {
				f.rules[i] = model
				replaced = true
				break
			}
		}
		if !replaced {
			f.rules = append(f.rules, model)
		}
		f.ruleCreates++
	case req.Method == http.MethodDelete && strings.HasPrefix(req.URL.Path, base+"/name/"):
		name := strings.TrimPrefix(req.URL.Path, base+"/name/")
		kept := f.rules[:0]
		for _, rule := range f.rules {
			if rule.Service.Name != name {
				kept = append(kept, rule)
			}
		}
		f.rules = kept
	default:
		http.Error(w, "unexpected request", http.StatusNotFound)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"result": "Success"})
}

func sameRuleKey(a, b loxiapi.LoadBalancerService) bool {
	return a.ExternalIP == b.ExternalIP && a.Port == b.Port && a.Protocol == b.Protocol &&
		a.Host == b.Host && a.PathPrefix == b.PathPrefix && a.PathMatchMode == b.PathMatchMode
}

// serveSNI registers or removes an SNI certificate the way loxilb does, and returns the result.
func (f *fakeLoxilb) serveSNI(req *http.Request) string {
	var model loxiapi.SniCertModel
	if err := json.NewDecoder(req.Body).Decode(&model); err != nil || model.Hostname == "" {
		return "Error: invalid request"
	}
	host := model.Hostname
	_, registered := f.sni[host]

	if req.Method == http.MethodDelete {
		if !registered {
			return "Error: Certificate for " + host + " not found"
		}
		delete(f.sni, host)
		return "Success"
	}

	if registered {
		return "Error: Certificate already registered for " + host
	}
	certData, err := os.ReadFile(filepath.Join(f.certDir, host, "server.crt"))
	if err != nil {
		return "Error: Failed to load certificate for " + host
	}
	if f.sni == nil {
		f.sni = map[string]string{}
	}
	f.sni[host] = string(certData)
	f.sniRegistrations++
	return "Success"
}

func (f *fakeLoxilb) client(t *testing.T) *loxiapi.LoxiClient {
	t.Helper()
	u, err := url.Parse(f.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	rc, err := loxiapi.NewRESTClient(u, "netlox", "v1", f.srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	return &loxiapi.LoxiClient{RestClient: rc, Host: "10.0.0.1"}
}

func (f *fakeLoxilb) paths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []string{}
	for _, rule := range f.rules {
		out = append(out, rule.Service.PathPrefix)
	}
	return out
}

func testIngress(path string, generation int64) *netv1.Ingress {
	pathType := netv1.PathTypePrefix
	className := loxilbIngressClassName
	return &netv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNs, Name: "web", Generation: generation},
		Spec: netv1.IngressSpec{
			IngressClassName: &className,
			Rules: []netv1.IngressRule{{
				Host: "www.example.com",
				IngressRuleValue: netv1.IngressRuleValue{HTTP: &netv1.HTTPIngressRuleValue{
					Paths: []netv1.HTTPIngressPath{{
						Path:     path,
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
}

type testPod struct {
	r       *LoxilbIngressReconciler
	loxilb  *fakeLoxilb
	markKey string
}

// newTestPods builds loxilb-ingress Pods that share one API server but each have their own loxilb.
func newTestPods(t *testing.T, names []string, objs ...client.Object) []testPod {
	t.Helper()
	shared := newTestReconciler(t, objs...)
	pods := make([]testPod, 0, len(names))
	for _, name := range names {
		key, err := ProgrammedAnnotationKey(name)
		if err != nil {
			t.Fatal(err)
		}
		lb := newFakeLoxilb(t)
		pods = append(pods, testPod{
			r: &LoxilbIngressReconciler{
				Client:        shared.Client,
				Scheme:        shared.Scheme,
				LoxiClient:    lb.client(t),
				ProgrammedKey: key,
			},
			loxilb:  lb,
			markKey: key,
		})
	}
	return pods
}

var webReq = ctrl.Request{NamespacedName: types.NamespacedName{Namespace: testNs, Name: "web"}}

func getIngress(t *testing.T, c client.Client) *netv1.Ingress {
	t.Helper()
	ing := &netv1.Ingress{}
	if err := c.Get(context.Background(), webReq.NamespacedName, ing); err != nil {
		t.Fatal(err)
	}
	return ing
}

func assertMark(t *testing.T, ing *netv1.Ingress, key, want string) {
	t.Helper()
	got, ok := ing.Annotations[key]
	switch {
	case want == "" && ok:
		t.Errorf("%s = %q, want no mark", key, got)
	case want != "" && got != want:
		t.Errorf("%s = %q (present=%v), want %q", key, got, ok, want)
	}
}

// assertRetrying reconciles the web Ingress and checks that it is retried soon because some
// backends couldn't be installed.
func assertRetrying(t *testing.T, r *LoxilbIngressReconciler, ctx context.Context) {
	t.Helper()
	res, err := r.Reconcile(ctx, webReq)
	if err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}
	if res.RequeueAfter != backendRetryInterval {
		t.Fatalf("RequeueAfter = %v, want %v", res.RequeueAfter, backendRetryInterval)
	}
}

func webBackend() []client.Object {
	return []client.Object{
		testService("web", corev1.ServicePort{Name: "http", Port: 80, TargetPort: intstr.FromInt32(8000)}),
		testSlice("web", "web-1", []slicePort{{name: strPtr("http"), port: 8000}}, "10.1.0.1"),
	}
}

func TestProgrammedReportPerPod(t *testing.T) {
	ctx := context.Background()
	pods := newTestPods(t, []string{"loxilb-ingress-aaaaa", "loxilb-ingress-bbbbb"},
		append(webBackend(), testIngress("/", 1))...)
	a, b := pods[0], pods[1]
	c := a.r.Client

	// Pod A installs: only A's mark appears, and only A's loxilb has the rule.
	if _, err := a.r.Reconcile(ctx, webReq); err != nil {
		t.Fatalf("pod A reconcile: %v", err)
	}
	ing := getIngress(t, c)
	assertMark(t, ing, a.markKey, "1")
	assertMark(t, ing, b.markKey, "")
	if got := b.loxilb.paths(); len(got) != 0 {
		t.Fatalf("pod B loxilb rules = %v, want none", got)
	}

	if _, err := b.r.Reconcile(ctx, webReq); err != nil {
		t.Fatalf("pod B reconcile: %v", err)
	}
	ing = getIngress(t, c)
	assertMark(t, ing, a.markKey, "1")
	assertMark(t, ing, b.markKey, "1")

	// The path changes and the generation goes up: each mark follows its own Pod's install.
	ing.Spec.Rules[0].HTTP.Paths[0].Path = "/v2"
	ing.Generation = 2
	if err := c.Update(ctx, ing); err != nil {
		t.Fatal(err)
	}

	if _, err := a.r.Reconcile(ctx, webReq); err != nil {
		t.Fatalf("pod A reconcile after update: %v", err)
	}
	ing = getIngress(t, c)
	assertMark(t, ing, a.markKey, "2")
	assertMark(t, ing, b.markKey, "1")
	if got := a.loxilb.paths(); len(got) != 1 || got[0] != "/v2" {
		t.Fatalf("pod A loxilb paths = %v, want [/v2]", got)
	}
	if got := b.loxilb.paths(); len(got) != 1 || got[0] != "/" {
		t.Fatalf("pod B loxilb paths = %v, want [/]", got)
	}

	if _, err := b.r.Reconcile(ctx, webReq); err != nil {
		t.Fatalf("pod B reconcile after update: %v", err)
	}
	ing = getIngress(t, c)
	assertMark(t, ing, a.markKey, "2")
	assertMark(t, ing, b.markKey, "2")
}

func TestProgrammedReportNotWrittenWithoutEndpoints(t *testing.T) {
	ctx := context.Background()
	pods := newTestPods(t, []string{"loxilb-ingress-aaaaa"},
		testService("web", corev1.ServicePort{Name: "http", Port: 80, TargetPort: intstr.FromInt32(8000)}),
		testIngress("/", 1),
	)
	a := pods[0]

	assertRetrying(t, a.r, ctx)
	assertMark(t, getIngress(t, a.r.Client), a.markKey, "")
}

func TestProgrammedReportClearedWhenRulesAreDropped(t *testing.T) {
	ctx := context.Background()
	slice := testSlice("web", "web-1", []slicePort{{name: strPtr("http"), port: 8000}}, "10.1.0.1")
	pods := newTestPods(t, []string{"loxilb-ingress-aaaaa"},
		testService("web", corev1.ServicePort{Name: "http", Port: 80, TargetPort: intstr.FromInt32(8000)}),
		slice,
		testIngress("/", 1),
	)
	a := pods[0]
	c := a.r.Client

	if _, err := a.r.Reconcile(ctx, webReq); err != nil {
		t.Fatal(err)
	}
	assertMark(t, getIngress(t, c), a.markKey, "1")

	// The endpoints go away: the rules are removed, and so is the mark, although the generation is unchanged.
	if err := c.Delete(ctx, slice); err != nil {
		t.Fatal(err)
	}
	assertRetrying(t, a.r, ctx)
	assertMark(t, getIngress(t, c), a.markKey, "")
	if got := a.loxilb.paths(); len(got) != 0 {
		t.Fatalf("loxilb rules = %v, want none", got)
	}
}

func TestProgrammedReportDisabledWithoutKey(t *testing.T) {
	ctx := context.Background()
	pods := newTestPods(t, []string{"loxilb-ingress-aaaaa"}, append(webBackend(), testIngress("/", 1))...)
	a := pods[0]
	a.r.ProgrammedKey = ""

	if _, err := a.r.Reconcile(ctx, webReq); err != nil {
		t.Fatal(err)
	}
	for k := range getIngress(t, a.r.Client).Annotations {
		if isProgrammedAnnotation(k) {
			t.Fatalf("unexpected mark %s", k)
		}
	}
}

func TestClearProgrammedAnnotations(t *testing.T) {
	ctx := context.Background()
	own, _ := ProgrammedAnnotationKey("loxilb-ingress-aaaaa")
	other, _ := ProgrammedAnnotationKey("loxilb-ingress-bbbbb")

	marked := testIngress("/", 3)
	marked.Annotations = map[string]string{own: "3", other: "3", "loxilb.io/epselect": "rr"}
	unmarked := testIngress("/", 1)
	unmarked.Name = "other"
	unmarked.Annotations = map[string]string{other: "1"}

	r := newTestReconciler(t, marked, unmarked)
	if err := ClearProgrammedAnnotations(ctx, r.Client, r.Client, own); err != nil {
		t.Fatal(err)
	}

	got := &netv1.Ingress{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(marked), got); err != nil {
		t.Fatal(err)
	}
	assertMark(t, got, own, "")
	assertMark(t, got, other, "3")
	if got.Annotations["loxilb.io/epselect"] != "rr" {
		t.Errorf("other annotations changed: %v", got.Annotations)
	}

	if err := r.Get(ctx, client.ObjectKeyFromObject(unmarked), got); err != nil {
		t.Fatal(err)
	}
	assertMark(t, got, other, "1")
}

func TestProgrammedAnnotationKey(t *testing.T) {
	key, err := ProgrammedAnnotationKey("loxilb-ingress-x7k2p")
	if err != nil || key != "loxilb.io/programmed.loxilb-ingress-x7k2p" {
		t.Fatalf("key = %q, err = %v", key, err)
	}

	// The part after "loxilb.io/" is limited to 63 characters.
	if _, err := ProgrammedAnnotationKey(strings.Repeat("a", 53)); err == nil {
		t.Fatal("expected error for a Pod name that does not fit")
	}
}

func TestOnlyOthersMarksChanged(t *testing.T) {
	own, _ := ProgrammedAnnotationKey("loxilb-ingress-aaaaa")
	other, _ := ProgrammedAnnotationKey("loxilb-ingress-bbbbb")

	base := func() *netv1.Ingress {
		ing := testIngress("/", 2)
		ing.ResourceVersion = "10"
		ing.Annotations = map[string]string{"loxilb.io/epselect": "rr", own: "2"}
		return ing
	}

	tests := []struct {
		name   string
		mutate func(*netv1.Ingress)
		want   bool
	}{
		{"other Pod's mark added", func(i *netv1.Ingress) { i.Annotations[other] = "2" }, true},
		{"own mark changed by someone else to another generation", func(i *netv1.Ingress) {
			i.Annotations[own] = "1"
		}, false},
		{"own mark removed by someone else", func(i *netv1.Ingress) { delete(i.Annotations, own) }, false},
		{"other annotation changed with a mark", func(i *netv1.Ingress) {
			i.Annotations[other] = "2"
			i.Annotations["loxilb.io/epselect"] = "hash"
		}, false},
		{"spec changed with a mark", func(i *netv1.Ingress) {
			i.Annotations[other] = "2"
			i.Spec.Rules[0].HTTP.Paths[0].Path = "/v2"
		}, false},
		{"nothing changed (resync)", func(i *netv1.Ingress) {}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldIng, newIng := base(), base()
			newIng.ResourceVersion = "11"
			tt.mutate(newIng)
			if got := onlyOthersMarksChanged(oldIng, newIng, own); got != tt.want {
				t.Fatalf("onlyOthersMarksChanged = %v, want %v", got, tt.want)
			}
		})
	}

	// This Pod's own patch: the mark moves to the current generation.
	oldIng, newIng := base(), base()
	oldIng.Annotations[own] = "1"
	newIng.ResourceVersion = "11"
	if !onlyOthersMarksChanged(oldIng, newIng, own) {
		t.Fatal("own patch to the current generation should not trigger a reconcile")
	}
}

func TestCreateDirectLoxiModelListUsesEndpointSlicePorts(t *testing.T) {
	ingress := testIngress("/", 1)
	ingress.Annotations = map[string]string{"loxilb.io/direct-loadbalance-service": "web"}

	r := newTestReconciler(t,
		testService("web",
			corev1.ServicePort{Name: "http", Port: 80, TargetPort: intstr.FromString("web")},
			corev1.ServicePort{Name: "dns", Port: 53, Protocol: corev1.ProtocolUDP, TargetPort: intstr.FromInt32(5353)},
		),
		testSlice("web", "web-1", []slicePort{
			{name: strPtr("http"), port: 8000},
			{name: strPtr("dns"), port: 5353, protocol: corev1.ProtocolUDP},
		}, "10.1.0.1"),
	)

	models, skipped, err := r.createDirectLoxiModelList(context.Background(), ingress)
	if err != nil || len(skipped) != 0 {
		t.Fatalf("unexpected error: %v, skipped: %v", err, skipped)
	}
	if len(models) != 2 {
		t.Fatalf("models = %d, want 2", len(models))
	}
	if models[0].Service.Port != 80 || models[1].Service.Port != 53 {
		t.Fatalf("frontend ports = %d, %d, want 80, 53", models[0].Service.Port, models[1].Service.Port)
	}
	assertEndpoints(t, models[0].Endpoints, "10.1.0.1:8000")
	assertEndpoints(t, models[1].Endpoints, "10.1.0.1:5353")
}
