package managers

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"loxilb.io/loxilb-ingress-manager/pkg"
)

func ruleTimeouts(lb *fakeLoxilb) []uint32 {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	out := []uint32{}
	for _, rule := range lb.rules {
		out = append(out, rule.Service.Timeout)
	}
	return out
}

func ruleCreates(lb *fakeLoxilb) int {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	return lb.ruleCreates
}

func TestInactiveTimeout(t *testing.T) {
	tests := []struct {
		value string
		set   bool
		want  uint32
	}{
		{set: false, want: defaultInactiveTimeout},
		{value: "3600", set: true, want: 3600},
		{value: " 600 ", set: true, want: 600},
		{value: "86400", set: true, want: 86400},
		{value: "86401", set: true, want: maxInactiveTimeout},
		{value: "0", set: true, want: defaultInactiveTimeout},
		{value: "-5", set: true, want: defaultInactiveTimeout},
		{value: "30m", set: true, want: defaultInactiveTimeout},
		{value: "", set: true, want: defaultInactiveTimeout},
	}

	for _, tt := range tests {
		ing := testIngress("/", 1)
		if tt.set {
			ing.Annotations = map[string]string{pkg.TimeoutAnnotation: tt.value}
		}
		if got := inactiveTimeout(context.Background(), ing); got != tt.want {
			t.Errorf("timeout for annotation %q (set=%v) = %d, want %d", tt.value, tt.set, got, tt.want)
		}
	}
}

func TestTimeoutAnnotationSetsRuleTimeout(t *testing.T) {
	ctx := context.Background()
	pods := newTestPods(t, []string{"loxilb-ingress-aaaaa"}, append(webBackend(), testIngress("/", 1))...)
	a := pods[0]
	c := a.r.Client

	// Without the annotation the rule gets the default, and it isn't installed again while
	// nothing changes.
	for i := 0; i < 2; i++ {
		if _, err := a.r.Reconcile(ctx, webReq); err != nil {
			t.Fatal(err)
		}
	}
	if got := ruleTimeouts(a.loxilb); len(got) != 1 || got[0] != defaultInactiveTimeout {
		t.Fatalf("rule timeouts = %v, want [%d]", got, defaultInactiveTimeout)
	}
	if got := ruleCreates(a.loxilb); got != 1 {
		t.Fatalf("rule was created %d times, want once", got)
	}

	// The annotation is a metadata change only; the rule follows it.
	ing := getIngress(t, c)
	ing.Annotations[pkg.TimeoutAnnotation] = "7200"
	if err := c.Update(ctx, ing); err != nil {
		t.Fatal(err)
	}
	if _, err := a.r.Reconcile(ctx, webReq); err != nil {
		t.Fatal(err)
	}
	if got := ruleTimeouts(a.loxilb); len(got) != 1 || got[0] != 7200 {
		t.Fatalf("rule timeouts = %v, want [7200]", got)
	}
}

func TestDirectLoadBalanceRuleKeepsLoxilbTimeout(t *testing.T) {
	ctx := context.Background()
	ingress := testIngress("/", 1)
	ingress.Annotations = map[string]string{
		pkg.DirectLBServiceAnnotation: "web",
		pkg.TimeoutAnnotation:         "7200",
	}
	pods := newTestPods(t, []string{"loxilb-ingress-aaaaa"},
		testService("web", corev1.ServicePort{Name: "http", Port: 80, TargetPort: intstr.FromInt32(8000)}),
		testSlice("web", "web-1", []slicePort{{name: strPtr("http"), port: 8000}}, "10.1.0.1"),
		ingress,
	)
	a := pods[0]

	if _, err := a.r.Reconcile(ctx, webReq); err != nil {
		t.Fatal(err)
	}
	if got := ruleTimeouts(a.loxilb); len(got) != 1 || got[0] != 0 {
		t.Fatalf("rule timeouts = %v, want [0] (loxilb's default)", got)
	}

	// loxilb reports the timeout it applied. That must not look like a change.
	a.loxilb.mu.Lock()
	a.loxilb.rules[0].Service.Timeout = 240
	a.loxilb.mu.Unlock()
	if _, err := a.r.Reconcile(ctx, webReq); err != nil {
		t.Fatal(err)
	}
	if got := ruleCreates(a.loxilb); got != 1 {
		t.Fatalf("rule was created %d times, want once", got)
	}
}
