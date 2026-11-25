/*
 * Copyright (c) 2024 NetLOX Inc
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at:
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package cert

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	loxiapi "github.com/loxilb-io/kube-loxilb/pkg/api"
	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	certBaseDir = "/opt/loxilb/cert"
	certFile    = "server.crt"
	keyFile     = "server.key"
	// mtlsClientSubDir is the directory under the certificate directory that holds mTLS files.
	mtlsClientSubDir = "client"
)

type Manager struct {
	client.Client
	LoxiClient *loxiapi.LoxiClient
	// CertDir is the directory loxilb reads host certificates from, one subdirectory per host.
	CertDir string

	mu sync.Mutex
	// loaded holds, per host, the digest of the certificate and key loxilb was last told to load.
	// loxilb keeps a certificate in memory once it is registered, so the files alone don't tell
	// what it serves.
	loaded map[string][sha256.Size]byte
}

func NewManager(c client.Client, loxiClient *loxiapi.LoxiClient) *Manager {
	return &Manager{
		Client:     c,
		LoxiClient: loxiClient,
		CertDir:    certBaseDir,
		loaded:     make(map[string][sha256.Size]byte),
	}
}

// ProcessIngressTLS registers the certificates of the Ingress's TLS hosts with loxilb.
// providesHost tells whether this Ingress is the one that provides the certificate of a host;
// several Ingresses may list the same host, and loxilb keeps one certificate per host. For a
// host that another Ingress provides, this Ingress's certificate is registered only while the
// host has none, and never replaces the one in use.
func (m *Manager) ProcessIngressTLS(ctx context.Context, ingress *netv1.Ingress, providesHost func(host string) bool) error {
	logger := log.FromContext(ctx)

	if len(ingress.Spec.TLS) == 0 {
		logger.Info("no TLS configuration found in ingress", "ingress", ingress.Name)
		return nil
	}

	// A host listed in more than one TLS entry takes the certificate of the first one.
	done := make(map[string]struct{})

	for _, entry := range ingress.Spec.TLS {
		if entry.SecretName == "" {
			logger.Info("TLS entry has no secret name, skipping", "hosts", entry.Hosts)
			continue
		}

		// Get secret from Kubernetes
		secret, err := m.getSecret(ctx, ingress.Namespace, entry.SecretName)
		if err != nil {
			return fmt.Errorf("failed to get secret %s/%s: %w", ingress.Namespace, entry.SecretName, err)
		}

		// Extract certificate and key from secret
		certData, keyData, err := m.extractCertificateFromSecret(secret)
		if err != nil {
			return fmt.Errorf("failed to extract certificate from secret %s: %w", entry.SecretName, err)
		}

		for _, host := range entry.Hosts {
			if _, ok := done[host]; ok {
				continue
			}
			done[host] = struct{}{}

			mayReplace := providesHost == nil || providesHost(host)
			if err := m.ensureHostCertificate(ctx, host, certData, keyData, mayReplace); err != nil {
				return fmt.Errorf("host %s (secret %s): %w", host, entry.SecretName, err)
			}
		}
	}

	return nil
}

// ensureHostCertificate makes loxilb serve a certificate for host: certData and keyData, or the
// one it already serves when that differs and mayReplace is false.
//
// The certificate is registered on every call, because a restarted loxilb has lost it. loxilb
// refuses a host it already has and has no call to replace a certificate, so when the content
// differs from what it loaded, the certificate is removed and registered again. The host has no
// certificate of its own between the two calls.
//
// The files of a host are in place once this has returned without error. loxilb also reads them
// when it creates the HTTPS rule of the host.
func (m *Manager) ensureHostCertificate(ctx context.Context, host string, certData, keyData []byte, mayReplace bool) error {
	logger := log.FromContext(ctx)
	digest := certificateDigest(certData, keyData)
	sni := &loxiapi.SniCertModel{Hostname: host}

	m.mu.Lock()
	defer m.mu.Unlock()

	loaded, known := m.loaded[host]
	if known && loaded != digest && !mayReplace {
		// Another Ingress provides this host's certificate. The files hold what was loaded, so
		// registering puts it back if loxilb lost it and changes nothing otherwise.
		if err := m.LoxiClient.SniCert().Create(ctx, sni); err != nil && !sniAlreadyRegistered(err) {
			return fmt.Errorf("failed to call LoxiLB API create SNI certificate: %w", err)
		}
		return nil
	}
	changed := !known || loaded != digest

	if known && changed {
		// The certificate in use is removed before the new one is loaded, so a pair that can't
		// be loaded would leave the host without one. Keep the old one in that case.
		if _, err := tls.X509KeyPair(certData, keyData); err != nil {
			return fmt.Errorf("keeping the certificate in use: the new certificate and key are not a valid pair: %w", err)
		}
	}

	if changed {
		// loxilb reads the files only when the certificate is registered.
		if err := m.storeCertificateForHost(ctx, host, certData, keyData); err != nil {
			return fmt.Errorf("failed to store certificate: %w", err)
		}
	}

	err := m.LoxiClient.SniCert().Create(ctx, sni)
	if err == nil {
		m.loaded[host] = digest
		logger.Info("registered SNI certificate", "host", host)
		return nil
	}
	if !sniAlreadyRegistered(err) {
		return fmt.Errorf("failed to call LoxiLB API create SNI certificate: %w", err)
	}
	if !changed {
		return nil
	}

	logger.Info("certificate changed. registering it again", "host", host)
	// A certificate that is gone by now, as after a loxilb restart, needs no removing.
	if err := m.LoxiClient.SniCert().Delete(ctx, sni); err != nil && !sniNotRegistered(err) {
		return fmt.Errorf("failed to call LoxiLB API delete SNI certificate: %w", err)
	}
	delete(m.loaded, host)
	if err := m.LoxiClient.SniCert().Create(ctx, sni); err != nil {
		return fmt.Errorf("failed to call LoxiLB API create SNI certificate: %w", err)
	}
	m.loaded[host] = digest

	return nil
}

// loxilb tells the outcome of an SNI call in the text of its result, and the client hands that
// text on as the error. It may come with HTTP 200 or with an error status, depending on the
// loxilb build; the text is the same.

// sniAlreadyRegistered reports whether registering failed because the host has a certificate.
func sniAlreadyRegistered(err error) bool {
	return strings.Contains(err.Error(), "already registered")
}

// sniNotRegistered reports whether removing failed because the host has no certificate.
func sniNotRegistered(err error) bool {
	return strings.Contains(err.Error(), "not found")
}

func certificateDigest(certData, keyData []byte) [sha256.Size]byte {
	h := sha256.New()
	h.Write(certData)
	h.Write([]byte{0})
	h.Write(keyData)

	var digest [sha256.Size]byte
	copy(digest[:], h.Sum(nil))
	return digest
}

// getSecret retrieves a secret from Kubernetes
func (m *Manager) getSecret(ctx context.Context, namespace, name string) (*corev1.Secret, error) {
	logger := log.Log.WithName("cert-manager")
	secret := &corev1.Secret{}
	key := types.NamespacedName{
		Namespace: namespace,
		Name:      name,
	}

	logger.Info("attempting to get secret", "namespace", namespace, "name", name)
	if err := m.Get(ctx, key, secret); err != nil {
		logger.Error(err, "failed to get secret from Kubernetes API", "namespace", namespace, "name", name)
		return nil, err
	}
	logger.Info("successfully retrieved secret", "namespace", namespace, "name", name)

	return secret, nil
}

// extractCertificateFromSecret extracts certificate and key data from Kubernetes secret
// Supports both kubernetes.io/tls and Opaque secret types
func (m *Manager) extractCertificateFromSecret(secret *corev1.Secret) ([]byte, []byte, error) {
	logger := log.Log.WithName("cert-manager")
	var certData, keyData []byte
	var certExists, keyExists bool

	// Try standard TLS secret keys first (kubernetes.io/tls type)
	certData, certExists = secret.Data["tls.crt"]
	keyData, keyExists = secret.Data["tls.key"]

	// If not found, try alternative keys commonly used in Opaque secrets
	if !certExists {
		for _, certKey := range []string{"cert", "certificate", "tls.cert", "server.crt"} {
			if data, ok := secret.Data[certKey]; ok {
				certData = data
				certExists = true
				break
			}
		}
	}

	if !keyExists {
		for _, keyKey := range []string{"key", "private-key", "privatekey", "tls.private.key", "server.key"} {
			if data, ok := secret.Data[keyKey]; ok {
				keyData = data
				keyExists = true
				break
			}
		}
	}

	if !certExists {
		logger.Error(nil, "certificate not found in secret", "secret", secret.Name)
		return nil, nil, fmt.Errorf("certificate not found in secret %s (tried keys: tls.crt, cert, certificate, tls.cert, server.crt)", secret.Name)
	}

	if !keyExists {
		logger.Error(nil, "private key not found in secret", "secret", secret.Name)
		return nil, nil, fmt.Errorf("private key not found in secret %s (tried keys: tls.key, key, private-key, privatekey, tls.private.key, server.key)", secret.Name)
	}

	if len(certData) == 0 {
		logger.Error(nil, "certificate data is empty in secret", "secret", secret.Name)
		return nil, nil, fmt.Errorf("certificate data is empty in secret %s", secret.Name)
	}

	if len(keyData) == 0 {
		logger.Error(nil, "private key data is empty in secret", "secret", secret.Name)
		return nil, nil, fmt.Errorf("private key data is empty in secret %s", secret.Name)
	}

	return certData, keyData, nil
}

// storeCertificateForHost stores certificate and key files for a specific hostname
func (m *Manager) storeCertificateForHost(ctx context.Context, hostname string, certData, keyData []byte) error {
	logger := log.FromContext(ctx)

	// Create host-specific directory
	hostDir := filepath.Join(m.CertDir, hostname)
	if err := os.MkdirAll(hostDir, 0755); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", hostDir, err)
	}

	// Write certificate file
	certPath := filepath.Join(hostDir, certFile)
	if err := os.WriteFile(certPath, certData, 0644); err != nil {
		return fmt.Errorf("failed to write certificate file %s: %w", certPath, err)
	}
	logger.Info("created certificate file", "path", certPath)

	// Write key file
	keyPath := filepath.Join(hostDir, keyFile)
	if err := os.WriteFile(keyPath, keyData, 0600); err != nil {
		return fmt.Errorf("failed to write key file %s: %w", keyPath, err)
	}
	logger.Info("created key file", "path", keyPath)

	return nil
}

// CleanupIngressCertificates removes the certificates of the given hosts from the filesystem and
// from loxilb. The caller leaves out hosts that another Ingress still uses.
func (m *Manager) CleanupIngressCertificates(ctx context.Context, httpsHostName []string) error {
	logger := log.FromContext(ctx)

	m.mu.Lock()
	defer m.mu.Unlock()

	if len(httpsHostName) != 0 {
		for _, httpsHostName := range httpsHostName {
			delete(m.loaded, httpsHostName)

			hostDir := filepath.Join(m.CertDir, httpsHostName)
			if err := os.RemoveAll(hostDir); err != nil {
				logger.Error(err, "failed to remove certificate directory", "host", httpsHostName, "path", hostDir)
			} else {
				logger.Info("removed certificate directory", "host", httpsHostName, "path", hostDir)
			}

			if err := m.LoxiClient.SniCert().Delete(ctx, &loxiapi.SniCertModel{Hostname: httpsHostName}); err != nil && !sniNotRegistered(err) {
				logger.Error(err, "failed to call LoxiLB API delete SNI certificate", "host", httpsHostName)
			}
		}
	}
	return nil
}

// getMtlsCertBasePath returns the base path for mTLS certificates
func (m *Manager) getMtlsCertBasePath(namespace, ingressName string) string {
	return filepath.Join(m.CertDir, mtlsClientSubDir, namespace, ingressName)
}

// StoreMtlsFrontendCert stores frontend mTLS CA certificate and returns the file path
func (m *Manager) StoreMtlsFrontendCert(ctx context.Context, namespace, ingressName string, certData []byte) (string, error) {
	logger := log.FromContext(ctx)

	// Create directory: /opt/loxilb/cert/client/{namespace}/{ingress-name}/frontend
	frontendDir := filepath.Join(m.getMtlsCertBasePath(namespace, ingressName), "frontend")
	if err := os.MkdirAll(frontendDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create frontend directory %s: %w", frontendDir, err)
	}

	// Write client-ca.crt file
	certPath := filepath.Join(frontendDir, "client-ca.crt")
	if err := os.WriteFile(certPath, certData, 0644); err != nil {
		return "", fmt.Errorf("failed to write frontend CA certificate file %s: %w", certPath, err)
	}
	logger.Info("created frontend CA certificate file", "path", certPath)

	return certPath, nil
}

// StoreMtlsBackendCerts stores backend mTLS certificates and returns the file paths
func (m *Manager) StoreMtlsBackendCerts(ctx context.Context, namespace, ingressName string, caCertData, clientCertData, clientKeyData []byte) (caPath, certPath, keyPath string, err error) {
	logger := log.FromContext(ctx)

	// Create directory: /opt/loxilb/cert/client/{namespace}/{ingress-name}/backend
	backendDir := filepath.Join(m.getMtlsCertBasePath(namespace, ingressName), "backend")
	if err := os.MkdirAll(backendDir, 0755); err != nil {
		return "", "", "", fmt.Errorf("failed to create backend directory %s: %w", backendDir, err)
	}

	// Write backend-ca.crt file (if provided)
	if len(caCertData) > 0 {
		caPath = filepath.Join(backendDir, "backend-ca.crt")
		if err := os.WriteFile(caPath, caCertData, 0644); err != nil {
			return "", "", "", fmt.Errorf("failed to write backend CA certificate file %s: %w", caPath, err)
		}
		logger.Info("created backend CA certificate file", "path", caPath)
	}

	// Write client.crt file
	if len(clientCertData) > 0 {
		certPath = filepath.Join(backendDir, "client.crt")
		if err := os.WriteFile(certPath, clientCertData, 0644); err != nil {
			return "", "", "", fmt.Errorf("failed to write backend client certificate file %s: %w", certPath, err)
		}
		logger.Info("created backend client certificate file", "path", certPath)
	}

	// Write client.key file
	if len(clientKeyData) > 0 {
		keyPath = filepath.Join(backendDir, "client.key")
		if err := os.WriteFile(keyPath, clientKeyData, 0600); err != nil {
			return "", "", "", fmt.Errorf("failed to write backend client key file %s: %w", keyPath, err)
		}
		logger.Info("created backend client key file", "path", keyPath)
	}

	return caPath, certPath, keyPath, nil
}

// CleanupIngressMtlsCertificates removes mTLS certificate directory for the ingress
func (m *Manager) CleanupIngressMtlsCertificates(ctx context.Context, namespace, ingressName string) error {
	logger := log.FromContext(ctx)

	mtlsDir := m.getMtlsCertBasePath(namespace, ingressName)
	if err := os.RemoveAll(mtlsDir); err != nil {
		logger.Error(err, "failed to remove mTLS certificate directory", "path", mtlsDir)
		return err
	}
	logger.Info("removed mTLS certificate directory", "path", mtlsDir)

	return nil
}
