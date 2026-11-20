package managers

import (
	"context"
	"net/http/httptest"
	"testing"

	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
)

func readyz(r *LoxilbIngressReconciler) error {
	return r.ReadyzCheck(httptest.NewRequest("GET", "/readyz", nil))
}

func TestReadyAfterFirstReconcileOfStartupIngresses(t *testing.T) {
	ctx := context.Background()

	broken := testIngress("/", 1)
	broken.Name = "broken"
	broken.Spec.Rules[0].HTTP.Paths[0].Backend.Service.Name = "missing"
	other := testIngress("/", 1)
	other.Name = "not-loxilb"
	otherClass := "nginx"
	other.Spec.IngressClassName = &otherClass

	pods := newTestPods(t, []string{"loxilb-ingress-aaaaa"}, append(webBackend(), testIngress("/", 1), broken, other)...)
	a := pods[0]

	if err := readyz(a.r); err == nil {
		t.Fatal("ready before the Ingresses were listed")
	}
	if err := a.r.listInitialIngresses(ctx); err != nil {
		t.Fatal(err)
	}
	if err := readyz(a.r); err == nil {
		t.Fatal("ready before any Ingress was reconciled")
	}

	if _, err := a.r.Reconcile(ctx, webReq); err != nil {
		t.Fatal(err)
	}
	if err := readyz(a.r); err == nil {
		t.Fatal("ready before the broken Ingress was reconciled")
	}

	// A failing Ingress counts once it has been tried, and the non-loxilb Ingress isn't waited for.
	if _, err := a.r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: testNs, Name: "broken"}}); err != nil {
		t.Fatal(err)
	}
	if err := readyz(a.r); err != nil {
		t.Fatalf("not ready after the first pass: %v", err)
	}

	// Ingresses created later don't affect readiness.
	late := testIngress("/", 1)
	late.Name = "late"
	if err := a.r.Create(ctx, late); err != nil {
		t.Fatal(err)
	}
	if err := readyz(a.r); err != nil {
		t.Fatalf("not ready after a new Ingress: %v", err)
	}
}

func TestReconcileWithoutLoxilbDoesNotCount(t *testing.T) {
	ctx := context.Background()
	pods := newTestPods(t, []string{"loxilb-ingress-aaaaa"}, append(webBackend(), testIngress("/", 1))...)
	a := pods[0]
	if err := a.r.listInitialIngresses(ctx); err != nil {
		t.Fatal(err)
	}

	a.loxilb.mu.Lock()
	a.loxilb.failing = true
	a.loxilb.mu.Unlock()
	if _, err := a.r.Reconcile(ctx, webReq); err == nil {
		t.Fatal("expected reconcile error while loxilb fails")
	}
	if ok, _ := a.r.initialSync.done(); ok {
		t.Fatal("a reconcile that never reached loxilb counted toward readiness")
	}

	a.loxilb.mu.Lock()
	a.loxilb.failing = false
	a.loxilb.mu.Unlock()
	if _, err := a.r.Reconcile(ctx, webReq); err != nil {
		t.Fatal(err)
	}
	if err := readyz(a.r); err != nil {
		t.Fatalf("not ready: %v", err)
	}
}

func TestReconcileBeforeListCounts(t *testing.T) {
	var s initialSync
	web := types.NamespacedName{Namespace: testNs, Name: "web"}
	api := types.NamespacedName{Namespace: testNs, Name: "api"}

	// The controller can reconcile an Ingress before the startup list is taken.
	s.reconciled(web)
	s.expect([]types.NamespacedName{web, api})
	if ok, _ := s.done(); ok {
		t.Fatal("done while api is pending")
	}
	s.reconciled(api)
	if ok, reason := s.done(); !ok {
		t.Fatalf("not done: %s", reason)
	}
}

func TestReadyWithNoIngresses(t *testing.T) {
	pods := newTestPods(t, []string{"loxilb-ingress-aaaaa"})
	a := pods[0]
	if err := a.r.listInitialIngresses(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := readyz(a.r); err != nil {
		t.Fatalf("not ready: %v", err)
	}
}

func TestNotReadyWhenLoxilbIsDown(t *testing.T) {
	pods := newTestPods(t, []string{"loxilb-ingress-aaaaa"})
	a := pods[0]
	a.r.initialSync.expect(nil)
	if err := readyz(a.r); err != nil {
		t.Fatalf("not ready: %v", err)
	}

	a.loxilb.srv.Close()
	if err := readyz(a.r); err == nil {
		t.Fatal("ready while loxilb is down")
	}
}
