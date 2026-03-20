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
	"fmt"
	"os"
	"path/filepath"
	"strings"

	loxiapi "github.com/loxilb-io/kube-loxilb/pkg/api"
	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	certBaseDir       = "/opt/loxilb/cert"
	certFile          = "server.crt"
	keyFile           = "server.key"
	mtlsClientBaseDir = "/opt/loxilb/cert/client"
)

type Manager struct {
	client.Client
	LoxiClient *loxiapi.LoxiClient
}

func NewManager(c client.Client, loxiClient *loxiapi.LoxiClient) *Manager {
	return &Manager{
		Client:     c,
		LoxiClient: loxiClient,
	}
}

// ProcessIngressTLS processes TLS configuration from Ingress and stores certificates to filesystem
func (m *Manager) ProcessIngressTLS(ctx context.Context, ingress *netv1.Ingress) error {
	logger := log.FromContext(ctx)

	if len(ingress.Spec.TLS) == 0 {
		logger.Info("no TLS configuration found in ingress", "ingress", ingress.Name)
		return nil
	}

	for _, tls := range ingress.Spec.TLS {
		if tls.SecretName == "" {
			logger.Info("TLS entry has no secret name, skipping", "hosts", tls.Hosts)
			continue
		}

		// Get secret from Kubernetes
		secret, err := m.getSecret(ctx, ingress.Namespace, tls.SecretName)
		if err != nil {
			return fmt.Errorf("failed to get secret %s/%s: %w", ingress.Namespace, tls.SecretName, err)
		}

		// Extract certificate and key from secret
		certData, keyData, err := m.extractCertificateFromSecret(secret)
		if err != nil {
			return fmt.Errorf("failed to extract certificate from secret %s: %w", tls.SecretName, err)
		}

		// Store certificates for each host
		for _, host := range tls.Hosts {
			if err := m.storeCertificateForHost(ctx, host, certData, keyData); err != nil {
				return fmt.Errorf("failed to store certificate for host %s: %w", host, err)
			}
			logger.Info("stored certificate for host", "host", host, "secret", tls.SecretName)
			if err := m.LoxiClient.SniCert().Create(ctx, &loxiapi.SniCertModel{Hostname: host}); err != nil {
				// Ignore error if the certificate is already registered
				if !strings.Contains(err.Error(), "already registered") {
					return fmt.Errorf("failed to call LoxiLB API create SNI certificate for host %s: %w", host, err)
				}
				logger.Info("SNI certificate already registered, skipping", "host", host)
			}
		}
	}

	return nil
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
	hostDir := filepath.Join(certBaseDir, hostname)
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

// CleanupIngressCertificates removes certificate files for all hosts in the ingress
func (m *Manager) CleanupIngressCertificates(ctx context.Context, httpsHostName []string) error {
	logger := log.FromContext(ctx)

	if len(httpsHostName) != 0 {
		for _, httpsHostName := range httpsHostName {
			hostDir := filepath.Join(certBaseDir, httpsHostName)
			if err := os.RemoveAll(hostDir); err != nil {
				logger.Error(err, "failed to remove certificate directory", "host", httpsHostName, "path", hostDir)
			} else {
				logger.Info("removed certificate directory", "host", httpsHostName, "path", hostDir)
			}

			if err := m.LoxiClient.SniCert().Delete(ctx, &loxiapi.SniCertModel{Hostname: httpsHostName}); err != nil {
				logger.Error(err, "failed to call LoxiLB API delete SNI certificate", "host", httpsHostName)
			}
		}
	}
	return nil
}

// getMtlsCertBasePath returns the base path for mTLS certificates
func (m *Manager) getMtlsCertBasePath(namespace, ingressName string) string {
	return filepath.Join(mtlsClientBaseDir, namespace, ingressName)
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
