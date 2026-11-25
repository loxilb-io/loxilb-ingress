package managers

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"loxilb.io/loxilb-ingress-manager/pkg"
)

// dupIngresses returns two Ingresses with the same host and path: dup-old to the web Service and
// the newer dup-new to the api Service. The names sort the other way round than the ages.
func dupIngresses() (older, newer *netv1.Ingress) {
	older = namedIngress("dup-old", "web")
	older.CreationTimestamp = metav1.NewTime(time.Unix(2000, 0))
	newer = namedIngress("dup-new", "api")
	newer.CreationTimestamp = metav1.NewTime(time.Unix(3000, 0))
	return older, newer
}

func apiBackend() []client.Object {
	return []client.Object{
		testService("api", corev1.ServicePort{Name: "http", Port: 80, TargetPort: intstr.FromInt32(9000)}),
		testSlice("api", "api-1", []slicePort{{name: strPtr("http"), port: 9000}}, "10.2.0.1"),
	}
}

func newDupTestPod(t *testing.T, objs ...client.Object) (testPod, *record.FakeRecorder) {
	t.Helper()
	objs = append(objs, webBackend()...)
	objs = append(objs, apiBackend()...)
	pod := newTestPods(t, []string{"loxilb-ingress-aaaaa"}, objs...)[0]
	recorder := record.NewFakeRecorder(100)
	pod.r.Recorder = recorder
	return pod, recorder
}

// installedRules returns "name path -> endpoints" of every rule, sorted.
func installedRules(lb *fakeLoxilb) []string {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	out := []string{}
	for _, rule := range lb.rules {
		out = append(out, rule.Service.Name+" "+rule.Service.PathPrefix+" -> "+strings.Join(epStrings(rule.Endpoints), ","))
	}
	sort.Strings(out)
	return out
}

func assertRules(t *testing.T, lb *fakeLoxilb, want ...string) {
	t.Helper()
	got := installedRules(lb)
	if strings.Join(got, "; ") != strings.Join(want, "; ") {
		t.Fatalf("loxilb rules = %v, want %v", got, want)
	}
}

func ingressNamed(t *testing.T, c client.Client, name string) *netv1.Ingress {
	t.Helper()
	ing := &netv1.Ingress{}
	if err := c.Get(context.Background(), reqFor(name).NamespacedName, ing); err != nil {
		t.Fatal(err)
	}
	return ing
}

func drainEvents(recorder *record.FakeRecorder) []string {
	events := []string{}
	for {
		select {
		case e := <-recorder.Events:
			events = append(events, e)
		default:
			return events
		}
	}
}

const (
	oldRule = "default_dup-old / -> 10.1.0.1:8000"
	newRule = "default_dup-new / -> 10.2.0.1:9000"
)

func TestOldestIngressGetsSharedPath(t *testing.T) {
	ctx := context.Background()
	older, newer := dupIngresses()
	pod, recorder := newDupTestPod(t, older, newer)

	// The newer Ingress is reconciled first. It leaves the path to the older one, which isn't
	// installed yet, and isn't retried like a backend that isn't ready.
	res, err := pod.r.Reconcile(ctx, reqFor("dup-new"))
	if err != nil {
		t.Fatal(err)
	}
	if res.RequeueAfter != resyncInterval {
		t.Fatalf("RequeueAfter = %v, want %v", res.RequeueAfter, resyncInterval)
	}
	assertRules(t, pod.loxilb)

	reconcileOK(t, pod.r, "dup-old")
	assertRules(t, pod.loxilb, oldRule)

	// From then on neither takes the rule from the other.
	settled := ruleCreates(pod.loxilb)
	for _, name := range []string{"dup-new", "dup-old", "dup-new", "dup-old"} {
		reconcileOK(t, pod.r, name)
		assertRules(t, pod.loxilb, oldRule)
	}
	if got := ruleCreates(pod.loxilb); got != settled {
		t.Fatalf("rule was created %d more times, want none", got-settled)
	}

	// Only the Ingress that serves the path reports as programmed, and the other one says why.
	assertMark(t, ingressNamed(t, pod.r.Client, "dup-old"), pod.markKey, "1")
	assertMark(t, ingressNamed(t, pod.r.Client, "dup-new"), pod.markKey, "")
	events := drainEvents(recorder)
	if len(events) == 0 {
		t.Fatal("no event for the Ingress that lost the path")
	}
	for _, e := range events {
		if !strings.Contains(e, "Warning "+pathConflictReason) || !strings.Contains(e, "default/dup-old") {
			t.Fatalf("unexpected event %q", e)
		}
	}
}

func TestSharedPathGoesToNextIngressWhenOwnerIsDeleted(t *testing.T) {
	for _, order := range [][]string{{"dup-old", "dup-new"}, {"dup-new", "dup-old"}} {
		t.Run("reconciled "+strings.Join(order, " then "), func(t *testing.T) {
			older, newer := dupIngresses()
			pod, _ := newDupTestPod(t, older, newer)
			reconcileOK(t, pod.r, "dup-old")
			reconcileOK(t, pod.r, "dup-new")
			assertRules(t, pod.loxilb, oldRule)

			// Whichever is reconciled first after the delete, the remaining Ingress ends up with the rule.
			deleteIngress(t, pod.r.Client, "dup-old")
			for _, name := range order {
				reconcileOK(t, pod.r, name)
			}
			assertRules(t, pod.loxilb, newRule)
			assertMark(t, ingressNamed(t, pod.r.Client, "dup-new"), pod.markKey, "1")
		})
	}
}

func TestSharedPathLeavesOtherPathsInstalled(t *testing.T) {
	ctx := context.Background()
	older, newer := dupIngresses()
	paths := &newer.Spec.Rules[0].HTTP.Paths
	own := (*paths)[0]
	own.Path = "/own"
	*paths = append(*paths, own)
	pod, _ := newDupTestPod(t, older, newer)

	reconcileOK(t, pod.r, "dup-old")
	res, err := pod.r.Reconcile(ctx, reqFor("dup-new"))
	if err != nil {
		t.Fatal(err)
	}
	if res.RequeueAfter != resyncInterval {
		t.Fatalf("RequeueAfter = %v, want %v", res.RequeueAfter, resyncInterval)
	}
	assertRules(t, pod.loxilb, "default_dup-new /own -> 10.2.0.1:9000", oldRule)
	// The shared path answers with the other Ingress's backend, so this one isn't reported.
	assertMark(t, ingressNamed(t, pod.r.Client, "dup-new"), pod.markKey, "")
}

func TestBackendNotReadyStillRetriesWithSharedPath(t *testing.T) {
	ctx := context.Background()
	older, newer := dupIngresses()
	paths := &newer.Spec.Rules[0].HTTP.Paths
	missing := (*paths)[0]
	missing.Path = "/missing"
	missing.Backend = netv1.IngressBackend{Service: &netv1.IngressServiceBackend{Name: "nowhere", Port: byNumber(80)}}
	*paths = append(*paths, missing)
	pod, _ := newDupTestPod(t, older, newer)

	res, err := pod.r.Reconcile(ctx, reqFor("dup-new"))
	if err != nil {
		t.Fatal(err)
	}
	if res.RequeueAfter != backendRetryInterval {
		t.Fatalf("RequeueAfter = %v, want %v", res.RequeueAfter, backendRetryInterval)
	}
}

func TestRulePathOwners(t *testing.T) {
	older, newer := dupIngresses()

	exact := namedIngress("exact", "web")
	exactType := netv1.PathTypeExact
	exact.Spec.Rules[0].HTTP.Paths[0].PathType = &exactType

	https := tlsIngress("https", "tls")
	https.Spec.Rules[0].Host = older.Spec.Rules[0].Host
	https.Spec.TLS[0].Hosts = []string{older.Spec.Rules[0].Host}

	otherClass := namedIngress("a-other-class", "web")
	otherClass.CreationTimestamp = metav1.NewTime(time.Unix(1000, 0))
	className := "nginx"
	otherClass.Spec.IngressClassName = &className

	direct := namedIngress("a-direct", "web")
	direct.CreationTimestamp = metav1.NewTime(time.Unix(1000, 0))
	direct.Annotations = map[string]string{pkg.DirectLBServiceAnnotation: "web"}

	r := newTestReconciler(t, older, newer, exact, https, otherClass, direct)
	owners, err := r.rulePathOwners(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	host := older.Spec.Rules[0].Host
	want := map[rulePath]string{
		// Older than dup-new; the Ingress of another class and the direct-loadbalance one are
		// older still but install no such rule.
		{port: 80, host: host, path: "/", matchMode: "prefix"}: "dup-old",
		// Another path type and another port are other rules.
		{port: 80, host: host, path: "/", matchMode: "exact"}:   "exact",
		{port: 443, host: host, path: "/", matchMode: "prefix"}: "https",
	}
	if len(owners) != len(want) {
		t.Fatalf("owners = %v, want %d entries", owners, len(want))
	}
	for path, name := range want {
		if owners[path].Name != name {
			t.Errorf("owner of %s = %q, want %q", path, owners[path].Name, name)
		}
	}
}

// The owners are worked out from the Ingress alone, the rules from createLoxiModelList. Both have
// to name a rule the same way.
func TestIngressRulePathsMatchInstalledRules(t *testing.T) {
	plain := twoPathIngress()
	exactType := netv1.PathTypeExact
	plain.Spec.Rules[0].HTTP.Paths[1].PathType = &exactType

	https := tlsIngress("https", "tls")
	mixed := tlsIngress("mixed", "tls")
	mixed.Spec.Rules = append(mixed.Spec.Rules, *namedIngress("x", "web").Spec.Rules[0].DeepCopy())
	mixed.Spec.Rules[1].Host = "plain.example.com"

	for _, ing := range []*netv1.Ingress{plain, https, mixed} {
		objs := append(webBackend(), apiBackend()...)
		r := newTestReconciler(t, objs...)
		models, skipped, err := r.createLoxiModelList(context.Background(), ing, nil)
		if err != nil || len(skipped) != 0 {
			t.Fatalf("%s: err = %v, skipped = %v", ing.Name, err, skipped)
		}

		installed := map[rulePath]int{}
		for _, model := range models {
			installed[rulePathOf(model.Service)]++
		}
		asked := map[rulePath]int{}
		for _, path := range r.ingressRulePaths(ing) {
			asked[path]++
		}
		if len(installed) != len(asked) || len(installed) == 0 {
			t.Fatalf("%s: installed %v, asked %v", ing.Name, installed, asked)
		}
		for path, n := range installed {
			if asked[path] != n {
				t.Fatalf("%s: installed %v, asked %v", ing.Name, installed, asked)
			}
		}
	}
}

func TestIngressesSharingRulePaths(t *testing.T) {
	older, newer := dupIngresses()
	unrelated := namedIngress("unrelated", "web")
	unrelated.Spec.Rules[0].Host = "other.example.com"
	otherClass := namedIngress("other-class", "web")
	className := "nginx"
	otherClass.Spec.IngressClassName = &className

	r := newTestReconciler(t, older, newer, unrelated, otherClass)
	names := func(obj client.Object) []string {
		out := []string{}
		for _, req := range r.ingressesSharingRulePaths(context.Background(), obj) {
			out = append(out, req.Name)
		}
		sort.Strings(out)
		return out
	}

	if got := names(older); len(got) != 1 || got[0] != "dup-new" {
		t.Fatalf("Ingresses sharing with dup-old = %v, want [dup-new]", got)
	}
	if got := names(unrelated); len(got) != 0 {
		t.Fatalf("Ingresses sharing with unrelated = %v, want none", got)
	}
	// An Ingress that left the class gives its paths up: the loxilb Ingresses that ask for them
	// are reconciled.
	if got := names(otherClass); len(got) != 2 {
		t.Fatalf("Ingresses sharing with the Ingress of another class = %v, want both", got)
	}
}
