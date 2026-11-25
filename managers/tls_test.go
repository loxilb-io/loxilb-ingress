package managers

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"loxilb.io/loxilb-ingress-manager/pkg/cert"
)

const tlsHost = "www.example.com"

// testKeyPair returns a self-signed certificate and its key, PEM encoded.
func testKeyPair(t *testing.T, commonName string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: commonName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{tlsHost},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

func tlsSecret(name string, certPEM, keyPEM []byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNs, Name: name},
		Type:       corev1.SecretTypeTLS,
		Data:       map[string][]byte{"tls.crt": certPEM, "tls.key": keyPEM},
	}
}

// tlsIngress serves tlsHost over HTTPS with the certificate in the Secret.
func tlsIngress(name, secret string) *netv1.Ingress {
	ing := testIngress("/", 1)
	ing.Name = name
	ing.Spec.Rules[0].Host = tlsHost
	ing.Spec.TLS = []netv1.IngressTLS{{Hosts: []string{tlsHost}, SecretName: secret}}
	return ing
}

func reqFor(name string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: testNs, Name: name}}
}

// newTLSTestPod builds a loxilb-ingress Pod whose certificates go to a temporary directory.
func newTLSTestPod(t *testing.T, objs ...client.Object) testPod {
	t.Helper()
	pod := newTestPods(t, []string{"loxilb-ingress-aaaaa"}, append(webBackend(), objs...)...)[0]
	dir := t.TempDir()
	pod.loxilb.certDir = dir
	pod.r.CertManager = cert.NewManager(pod.r.Client, pod.r.LoxiClient)
	pod.r.CertManager.CertDir = dir
	return pod
}

func reconcileOK(t *testing.T, r *LoxilbIngressReconciler, name string) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), reqFor(name)); err != nil {
		t.Fatalf("reconcile %s: %v", name, err)
	}
}

// servedCert returns the certificate loxilb serves for tlsHost, or "" when it has none.
func servedCert(lb *fakeLoxilb) string {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	return lb.sni[tlsHost]
}

func sniRegistrations(lb *fakeLoxilb) int {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	return lb.sniRegistrations
}

func deleteIngress(t *testing.T, c client.Client, name string) {
	t.Helper()
	ing := &netv1.Ingress{ObjectMeta: metav1.ObjectMeta{Namespace: testNs, Name: name}}
	if err := c.Delete(context.Background(), ing); err != nil {
		t.Fatal(err)
	}
}

func TestSharedTLSHostKeepsCertificateUntilLastIngressIsGone(t *testing.T) {
	certPEM, keyPEM := testKeyPair(t, "shared")
	pod := newTLSTestPod(t, tlsSecret("tls", certPEM, keyPEM), tlsIngress("cp", "tls"), tlsIngress("cluster-api", "tls"))
	certFile := filepath.Join(pod.loxilb.certDir, tlsHost, "server.crt")

	reconcileOK(t, pod.r, "cp")
	reconcileOK(t, pod.r, "cluster-api")
	if got := servedCert(pod.loxilb); got != string(certPEM) {
		t.Fatalf("served certificate = %q, want the Secret's", got)
	}

	// One of the two goes away: the other still serves the host, so the certificate stays.
	deleteIngress(t, pod.r.Client, "cp")
	reconcileOK(t, pod.r, "cp")
	if got := servedCert(pod.loxilb); got != string(certPEM) {
		t.Fatalf("certificate was removed while another Ingress still lists the host (served %q)", got)
	}
	if _, err := os.Stat(certFile); err != nil {
		t.Fatalf("certificate file was removed while another Ingress still lists the host: %v", err)
	}
	if got := sniRegistrations(pod.loxilb); got != 1 {
		t.Fatalf("certificate was registered %d times, want once", got)
	}

	// The last one goes away: now the certificate is removed.
	deleteIngress(t, pod.r.Client, "cluster-api")
	reconcileOK(t, pod.r, "cluster-api")
	if got := servedCert(pod.loxilb); got != "" {
		t.Fatalf("certificate still served after the last Ingress was deleted: %q", got)
	}
	if _, err := os.Stat(certFile); !os.IsNotExist(err) {
		t.Fatalf("certificate file still there after the last Ingress was deleted (err = %v)", err)
	}
}

func TestIngressLeavingLoxilbClassKeepsSharedCertificate(t *testing.T) {
	certPEM, keyPEM := testKeyPair(t, "shared")
	pod := newTLSTestPod(t, tlsSecret("tls", certPEM, keyPEM), tlsIngress("cp", "tls"), tlsIngress("cluster-api", "tls"))
	c := pod.r.Client

	reconcileOK(t, pod.r, "cp")
	reconcileOK(t, pod.r, "cluster-api")

	ing := &netv1.Ingress{}
	if err := c.Get(context.Background(), reqFor("cp").NamespacedName, ing); err != nil {
		t.Fatal(err)
	}
	other := "nginx"
	ing.Spec.IngressClassName = &other
	if err := c.Update(context.Background(), ing); err != nil {
		t.Fatal(err)
	}

	reconcileOK(t, pod.r, "cp")
	if got := servedCert(pod.loxilb); got != string(certPEM) {
		t.Fatalf("certificate was removed while another Ingress still lists the host (served %q)", got)
	}
}

func TestSecretChangeReloadsCertificate(t *testing.T) {
	ctx := context.Background()
	oldCert, oldKey := testKeyPair(t, "old")
	newCert, newKey := testKeyPair(t, "new")
	secret := tlsSecret("tls", oldCert, oldKey)
	pod := newTLSTestPod(t, secret, tlsIngress("web", "tls"))

	reconcileOK(t, pod.r, "web")
	reconcileOK(t, pod.r, "web")
	if got := servedCert(pod.loxilb); got != string(oldCert) {
		t.Fatalf("served certificate = %q, want the old one", got)
	}
	if got := sniRegistrations(pod.loxilb); got != 1 {
		t.Fatalf("unchanged certificate was registered %d times, want once", got)
	}

	// The certificate is renewed: loxilb has to load the new one.
	secret.Data = map[string][]byte{"tls.crt": newCert, "tls.key": newKey}
	if err := pod.r.Client.Update(ctx, secret); err != nil {
		t.Fatal(err)
	}
	reconcileOK(t, pod.r, "web")
	if got := servedCert(pod.loxilb); got != string(newCert) {
		t.Fatalf("served certificate after renewal = %q, want the new one", got)
	}

	reconcileOK(t, pod.r, "web")
	if got := sniRegistrations(pod.loxilb); got != 2 {
		t.Fatalf("certificate was registered %d times, want twice (once per content)", got)
	}
}

func TestBrokenRenewalKeepsCertificateInUse(t *testing.T) {
	ctx := context.Background()
	oldCert, oldKey := testKeyPair(t, "old")
	newCert, _ := testKeyPair(t, "new")
	secret := tlsSecret("tls", oldCert, oldKey)
	pod := newTLSTestPod(t, secret, tlsIngress("web", "tls"))

	reconcileOK(t, pod.r, "web")

	// The certificate is replaced but the key is not yet: the pair doesn't match.
	secret.Data = map[string][]byte{"tls.crt": newCert, "tls.key": oldKey}
	if err := pod.r.Client.Update(ctx, secret); err != nil {
		t.Fatal(err)
	}
	if _, err := pod.r.Reconcile(ctx, reqFor("web")); err == nil {
		t.Fatal("expected an error for a certificate and key that don't match")
	}
	if got := servedCert(pod.loxilb); got != string(oldCert) {
		t.Fatalf("served certificate = %q, want the old one to stay", got)
	}
}

func TestCertificateIsRegisteredAgainAfterLoxilbRestart(t *testing.T) {
	certPEM, keyPEM := testKeyPair(t, "web")
	pod := newTLSTestPod(t, tlsSecret("tls", certPEM, keyPEM), tlsIngress("web", "tls"))

	reconcileOK(t, pod.r, "web")

	// loxilb restarts and comes back empty.
	pod.loxilb.mu.Lock()
	pod.loxilb.sni = nil
	pod.loxilb.rules = nil
	pod.loxilb.mu.Unlock()

	reconcileOK(t, pod.r, "web")
	if got := servedCert(pod.loxilb); got != string(certPEM) {
		t.Fatalf("served certificate after restart = %q, want the Secret's", got)
	}
}

func TestOldestIngressProvidesSharedHostCertificate(t *testing.T) {
	certA, keyA := testKeyPair(t, "a")
	certB, keyB := testKeyPair(t, "b")
	older := tlsIngress("zz-older", "tls-a")
	older.CreationTimestamp = metav1.NewTime(time.Unix(1000, 0))
	newer := tlsIngress("aa-newer", "tls-b")
	newer.CreationTimestamp = metav1.NewTime(time.Unix(2000, 0))
	pod := newTLSTestPod(t, tlsSecret("tls-a", certA, keyA), tlsSecret("tls-b", certB, keyB), older, newer)

	// The newer Ingress comes first and the host has no certificate yet, so its certificate is
	// used until the older Ingress has been reconciled.
	reconcileOK(t, pod.r, "aa-newer")
	if got := servedCert(pod.loxilb); got != string(certB) {
		t.Fatalf("served certificate = %q, want the first Ingress's while the host had none", got)
	}
	reconcileOK(t, pod.r, "zz-older")
	if got := servedCert(pod.loxilb); got != string(certA) {
		t.Fatalf("served certificate = %q, want the older Ingress's", got)
	}

	// From then on the two don't replace each other's certificate.
	settled := sniRegistrations(pod.loxilb)
	for _, name := range []string{"aa-newer", "zz-older", "aa-newer", "zz-older"} {
		reconcileOK(t, pod.r, name)
	}
	if got := servedCert(pod.loxilb); got != string(certA) {
		t.Fatalf("served certificate = %q, want the older Ingress's", got)
	}
	if got := sniRegistrations(pod.loxilb); got != settled {
		t.Fatalf("certificate was registered %d more times, want none", got-settled)
	}

	// The older Ingress is deleted: the certificate stays until the other one takes over.
	deleteIngress(t, pod.r.Client, "zz-older")
	reconcileOK(t, pod.r, "zz-older")
	if got := servedCert(pod.loxilb); got != string(certA) {
		t.Fatalf("served certificate right after the delete = %q, want it kept", got)
	}
	reconcileOK(t, pod.r, "aa-newer")
	if got := servedCert(pod.loxilb); got != string(certB) {
		t.Fatalf("served certificate = %q, want the remaining Ingress's", got)
	}
}

// Newer loxilb builds answer a failed SNI call with an error status instead of 200. The result
// text, which is what the outcome is read from, stays the same.
func TestSNICallsWithErrorStatus(t *testing.T) {
	ctx := context.Background()
	oldCert, oldKey := testKeyPair(t, "old")
	newCert, newKey := testKeyPair(t, "new")
	secret := tlsSecret("tls", oldCert, oldKey)
	pod := newTLSTestPod(t, secret, tlsIngress("web", "tls"))
	pod.loxilb.sniStatusCodes = true

	// Registered again on every reconcile: "already registered" now comes as 409.
	reconcileOK(t, pod.r, "web")
	reconcileOK(t, pod.r, "web")
	if got := servedCert(pod.loxilb); got != string(oldCert) {
		t.Fatalf("served certificate = %q, want the old one", got)
	}

	// A renewal still removes and registers.
	secret.Data = map[string][]byte{"tls.crt": newCert, "tls.key": newKey}
	if err := pod.r.Client.Update(ctx, secret); err != nil {
		t.Fatal(err)
	}
	reconcileOK(t, pod.r, "web")
	if got := servedCert(pod.loxilb); got != string(newCert) {
		t.Fatalf("served certificate after renewal = %q, want the new one", got)
	}

	// loxilb loses the certificate between the refused registration and the removal: the
	// removal answers 404, and the new certificate is registered all the same.
	secret.Data = map[string][]byte{"tls.crt": oldCert, "tls.key": oldKey}
	if err := pod.r.Client.Update(ctx, secret); err != nil {
		t.Fatal(err)
	}
	pod.loxilb.mu.Lock()
	pod.loxilb.sniLoseOnConflict = true
	pod.loxilb.mu.Unlock()
	reconcileOK(t, pod.r, "web")
	if got := servedCert(pod.loxilb); got != string(oldCert) {
		t.Fatalf("served certificate after loxilb lost it = %q, want the Secret's", got)
	}
	pod.loxilb.mu.Lock()
	pod.loxilb.sniLoseOnConflict = false
	pod.loxilb.mu.Unlock()

	// The Ingress is deleted after loxilb came back empty: there is no certificate to remove
	// (404), and the files are cleaned up as usual.
	pod.loxilb.mu.Lock()
	pod.loxilb.sni = nil
	pod.loxilb.mu.Unlock()
	deleteIngress(t, pod.r.Client, "web")
	reconcileOK(t, pod.r, "web")
	if _, err := os.Stat(filepath.Join(pod.loxilb.certDir, tlsHost)); !os.IsNotExist(err) {
		t.Fatalf("certificate files still there after the Ingress was deleted (err = %v)", err)
	}
}
