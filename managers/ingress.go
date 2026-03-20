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

package managers

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	loxiapi "github.com/loxilb-io/kube-loxilb/pkg/api"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	netv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"loxilb.io/loxilb-ingress-manager/pkg"
	"loxilb.io/loxilb-ingress-manager/pkg/cert"
)

const (
	loxilbIngressClassName = "loxilb"

	// mTLS Frontend annotations
	mtlsFrontendModeAnnotation      = "loxilb.io/mtls-frontend-mode"
	mtlsFrontendSecretAnnotation    = "loxilb.io/mtls-frontend-secret"
	mtlsFrontendRequireCnAnnotation = "loxilb.io/mtls-frontend-require-cn"
	mtlsFrontendCnPatternAnnotation = "loxilb.io/mtls-frontend-cn-pattern"

	// mTLS Backend annotations
	mtlsBackendVerifyAnnotation       = "loxilb.io/mtls-backend-verify"
	mtlsBackendCaSecretAnnotation     = "loxilb.io/mtls-backend-ca-secret"
	mtlsBackendClientSecretAnnotation = "loxilb.io/mtls-backend-client-secret"
)

type LoxilbIngressReconciler struct {
	client.Client
	Scheme      *runtime.Scheme
	LoxiClient  *loxiapi.LoxiClient
	CertManager *cert.Manager
}

func isLoxilbIngress(ing *netv1.Ingress) bool {
	if ing.Spec.IngressClassName != nil {
		if *ing.Spec.IngressClassName == loxilbIngressClassName {
			return true
		}
	}

	return false
}

func (r *LoxilbIngressReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	currLBList, err := r.LoxiClient.LoadBalancer().List(ctx)
	if err != nil {
		logger.Info("Failed to get existing loxilb-ingress rules")
		return ctrl.Result{}, err
	}

	ingress := &netv1.Ingress{}
	err = r.Get(ctx, req.NamespacedName, ingress)
	if err != nil {
		// Ingress is deleted.
		if errors.IsNotFound(err) {
			logger.Info("This resource is deleted", "Ingress", req.NamespacedName)

			existingRules := r.getManagedIngressRules(currLBList.Item, req.NamespacedName, nil)
			return r.handleIngressDeleted(ctx, req.Namespace, req.Name, existingRules)
		}

		logger.Error(err, "Failed to get ingress", "ingress", ingress)
		return ctrl.Result{}, err
	}

	existingRules := r.getManagedIngressRules(currLBList.Item, req.NamespacedName, ingress)

	if !isLoxilbIngress(ingress) {
		logger.Info("Ingress no longer uses loxilb class. cleaning up managed rules", "Ingress", req.NamespacedName)
		return r.handleIngressDeleted(ctx, req.Namespace, req.Name, existingRules)
	}

	// Process TLS certificates before creating loxilb rules
	if r.CertManager != nil && len(ingress.Spec.TLS) > 0 {
		if err := r.CertManager.ProcessIngressTLS(ctx, ingress); err != nil {
			logger.Error(err, "failed to process TLS certificates", "ingress", ingress.Name)
			return ctrl.Result{}, err
		}
	}

	// when ingress is added, install rule to loxilb-ingress
	var models []loxiapi.LoadBalancerModel
	if _, isok := ingress.Annotations["loxilb.io/direct-loadbalance-service"]; isok {
		models, err = r.createDirectLoxiModelList(ctx, ingress)
	} else {
		models, err = r.createLoxiModelList(ctx, ingress)
	}

	if err != nil {
		if cleanupErr := r.deleteManagedRules(ctx, existingRules); cleanupErr != nil {
			logger.Error(cleanupErr, "failed to cleanup existing loxilb-ingress rules after model generation error")
		}
		logger.Error(err, "Failed to set ingress. failed to create loxilb loadbalancer model", "[]loxiapi.LoadBalancerModel", models)
		return ctrl.Result{}, err
	}

	if err := r.syncRules(ctx, existingRules, models); err != nil {
		logger.Error(err, "failed to synchronize loxilb-ingress rules", "Ingress", req.NamespacedName)
		return ctrl.Result{}, err
	}

	if err := r.updateIngressStatus(ctx, ingress); err != nil {
		logger.Info("failed to update ingress status.", "error", err)
	}

	logger.Info("Ingress reconciled successfully", "ingress", req.NamespacedName)
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

func buildManagedRuleNameSet(req types.NamespacedName, ingress *netv1.Ingress) map[string]struct{} {
	names := make(map[string]struct{})

	baseName := fmt.Sprintf("%s_%s", req.Namespace, req.Name)
	names[baseName] = struct{}{}
	names[baseName+"_https"] = struct{}{}

	if ingress != nil {
		if directNs, ok := ingress.Annotations["loxilb.io/direct-loadbalance-namespace"]; ok {
			if directNs != "" && directNs != req.Namespace {
				legacyBaseName := fmt.Sprintf("%s_%s", directNs, req.Name)
				names[legacyBaseName] = struct{}{}
				names[legacyBaseName+"_https"] = struct{}{}
			}
		}
	}

	return names
}

func (r *LoxilbIngressReconciler) getManagedIngressRules(lbItems []loxiapi.LoadBalancerModel, req types.NamespacedName, ingress *netv1.Ingress) []loxiapi.LoadBalancerModel {
	ruleNameSet := buildManagedRuleNameSet(req, ingress)
	rules := make([]loxiapi.LoadBalancerModel, 0)

	for _, lbItem := range lbItems {
		if _, ok := ruleNameSet[lbItem.Service.Name]; ok {
			rules = append(rules, lbItem)
		}
	}

	return rules
}

func uniqueRuleNames(rules []loxiapi.LoadBalancerModel) []string {
	nameSet := make(map[string]struct{})
	for _, rule := range rules {
		nameSet[rule.Service.Name] = struct{}{}
	}

	names := make([]string, 0, len(nameSet))
	for name := range nameSet {
		names = append(names, name)
	}
	sort.Strings(names)

	return names
}

func uniqueHTTPSHosts(rules []loxiapi.LoadBalancerModel) []string {
	hostSet := make(map[string]struct{})

	for _, rule := range rules {
		if strings.HasSuffix(rule.Service.Name, "_https") {
			if rule.Service.Host != "" {
				hostSet[rule.Service.Host] = struct{}{}
			}
		}
	}

	hosts := make([]string, 0, len(hostSet))
	for host := range hostSet {
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)

	return hosts
}

func (r *LoxilbIngressReconciler) deleteManagedRules(ctx context.Context, rules []loxiapi.LoadBalancerModel) error {
	logger := log.FromContext(ctx)

	for _, name := range uniqueRuleNames(rules) {
		if err := r.LoxiClient.LoadBalancer().DeleteByName(ctx, name); err != nil {
			logger.Error(err, "failed to delete loxilb-ingress rule", "name", name)
			return err
		}
		logger.Info("deleted loxilb-ingress rule", "name", name)
	}

	return nil
}

func (r *LoxilbIngressReconciler) handleIngressDeleted(ctx context.Context, namespace, ingressName string, existingRules []loxiapi.LoadBalancerModel) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	deleteErr := r.deleteManagedRules(ctx, existingRules)

	if r.CertManager != nil {
		httpsHosts := uniqueHTTPSHosts(existingRules)

		if err := r.CertManager.CleanupIngressCertificates(ctx, httpsHosts); err != nil {
			logger.Error(err, "failed to cleanup TLS certificates")
		}

		if err := r.CertManager.CleanupIngressMtlsCertificates(ctx, namespace, ingressName); err != nil {
			logger.Error(err, "failed to cleanup mTLS certificates")
		}
	}

	if deleteErr != nil {
		return ctrl.Result{}, deleteErr
	}

	return ctrl.Result{}, nil
}

func serviceConfigChanged(existing, desired loxiapi.LoadBalancerService) bool {
	if existing.ExternalIP != desired.ExternalIP {
		return true
	}
	if existing.Protocol != desired.Protocol {
		return true
	}
	if existing.Port != desired.Port {
		return true
	}
	if existing.Mode != desired.Mode {
		return true
	}
	if existing.Sel != desired.Sel {
		return true
	}
	if existing.Security != desired.Security {
		return true
	}
	if existing.Host != desired.Host {
		return true
	}
	if existing.PathPrefix != desired.PathPrefix {
		return true
	}
	if existing.PathMatchMode != desired.PathMatchMode {
		return true
	}
	if existing.BackendProtocol != desired.BackendProtocol {
		return true
	}

	if mtlsFrontendChanged(existing.MtlsFrontend, desired.MtlsFrontend) {
		return true
	}

	if mtlsBackendChanged(existing.MtlsBackend, desired.MtlsBackend) {
		return true
	}

	return false
}

func stringPtrEqual(existing, desired *string) bool {
	if existing == nil || desired == nil {
		return existing == nil && desired == nil
	}

	return *existing == *desired
}

func boolPtrEqual(existing, desired *bool) bool {
	if existing == nil || desired == nil {
		return existing == nil && desired == nil
	}

	return *existing == *desired
}

func mtlsFrontendChanged(existing, desired *loxiapi.MtlsFrontend) bool {
	if existing == nil || desired == nil {
		return existing != desired
	}

	if !stringPtrEqual(existing.ClientCertMode, desired.ClientCertMode) {
		return true
	}
	if existing.ClientCaPath != desired.ClientCaPath {
		return true
	}
	if !boolPtrEqual(existing.RequireClientCn, desired.RequireClientCn) {
		return true
	}
	if existing.ClientCnPattern != desired.ClientCnPattern {
		return true
	}

	return false
}

func mtlsBackendChanged(existing, desired *loxiapi.MtlsBackend) bool {
	if existing == nil || desired == nil {
		return existing != desired
	}

	if !boolPtrEqual(existing.VerifyServerCert, desired.VerifyServerCert) {
		return true
	}
	if existing.BackendCaPath != desired.BackendCaPath {
		return true
	}
	if existing.ClientCertPath != desired.ClientCertPath {
		return true
	}
	if existing.ClientKeyPath != desired.ClientKeyPath {
		return true
	}

	return false
}

func endpointIdentity(ep loxiapi.LoadBalancerEndpoint) string {
	return fmt.Sprintf("%s:%d:%d", ep.EndpointIP, ep.TargetPort, ep.Weight)
}

func endpointsChanged(existing []loxiapi.LoadBalancerEndpoint, desired []loxiapi.LoadBalancerEndpoint) bool {
	if len(existing) != len(desired) {
		return true
	}

	countByEndpoint := make(map[string]int)
	for _, ep := range existing {
		countByEndpoint[endpointIdentity(ep)]++
	}

	for _, ep := range desired {
		key := endpointIdentity(ep)
		if countByEndpoint[key] == 0 {
			return true
		}
		countByEndpoint[key]--
	}

	for _, count := range countByEndpoint {
		if count != 0 {
			return true
		}
	}

	return false
}

func endpointSignature(endpoints []loxiapi.LoadBalancerEndpoint) string {
	endpointKeys := make([]string, 0, len(endpoints))
	for _, ep := range endpoints {
		endpointKeys = append(endpointKeys, endpointIdentity(ep))
	}
	sort.Strings(endpointKeys)

	return strings.Join(endpointKeys, ",")
}

func ruleIdentityKey(service loxiapi.LoadBalancerService) string {
	return fmt.Sprintf("%s|%s|%s|%s|%d|%s|%d|%s|%d",
		service.Name,
		service.Host,
		service.PathPrefix,
		service.PathMatchMode,
		service.Security,
		service.Protocol,
		service.Port,
		service.BackendProtocol,
		service.Sel,
	)
}

func modelSortKey(model loxiapi.LoadBalancerModel) string {
	return ruleIdentityKey(model.Service) + "|" + endpointSignature(model.Endpoints)
}

func groupModelsByName(models []loxiapi.LoadBalancerModel) map[string][]loxiapi.LoadBalancerModel {
	grouped := make(map[string][]loxiapi.LoadBalancerModel)
	for _, model := range models {
		grouped[model.Service.Name] = append(grouped[model.Service.Name], model)
	}

	return grouped
}

func ruleGroupChanged(existingGroup, desiredGroup []loxiapi.LoadBalancerModel) bool {
	if len(existingGroup) != len(desiredGroup) {
		return true
	}

	existing := append([]loxiapi.LoadBalancerModel(nil), existingGroup...)
	desired := append([]loxiapi.LoadBalancerModel(nil), desiredGroup...)

	sort.Slice(existing, func(i, j int) bool {
		return modelSortKey(existing[i]) < modelSortKey(existing[j])
	})
	sort.Slice(desired, func(i, j int) bool {
		return modelSortKey(desired[i]) < modelSortKey(desired[j])
	})

	for i := range existing {
		if ruleIdentityKey(existing[i].Service) != ruleIdentityKey(desired[i].Service) {
			return true
		}

		if serviceConfigChanged(existing[i].Service, desired[i].Service) {
			return true
		}

		if endpointsChanged(existing[i].Endpoints, desired[i].Endpoints) {
			return true
		}
	}

	return false
}

func (r *LoxilbIngressReconciler) createRuleWithRetry(ctx context.Context, model *loxiapi.LoadBalancerModel, retryOnExists bool) error {
	err := r.LoxiClient.LoadBalancer().Create(ctx, model)
	if err == nil {
		return nil
	}

	if err.Error() != "lbrule-exists error" {
		return err
	}

	if !retryOnExists {
		return nil
	}

	for attempt := 0; attempt < 3; attempt++ {
		time.Sleep(100 * time.Millisecond)

		err = r.LoxiClient.LoadBalancer().Create(ctx, model)
		if err == nil {
			return nil
		}
		if err.Error() != "lbrule-exists error" {
			return err
		}
	}

	return err
}

func (r *LoxilbIngressReconciler) createRuleGroup(ctx context.Context, models []loxiapi.LoadBalancerModel, retryOnExists bool) error {
	orderedModels := append([]loxiapi.LoadBalancerModel(nil), models...)
	sort.Slice(orderedModels, func(i, j int) bool {
		return modelSortKey(orderedModels[i]) < modelSortKey(orderedModels[j])
	})

	for i := range orderedModels {
		if err := r.createRuleWithRetry(ctx, &orderedModels[i], retryOnExists); err != nil {
			return err
		}
	}

	return nil
}

func (r *LoxilbIngressReconciler) syncRules(
	ctx context.Context,
	existingRules []loxiapi.LoadBalancerModel,
	desiredModels []loxiapi.LoadBalancerModel,
) error {
	logger := log.FromContext(ctx)

	existingByName := groupModelsByName(existingRules)
	desiredByName := groupModelsByName(desiredModels)

	nameSet := make(map[string]struct{})
	for name := range existingByName {
		nameSet[name] = struct{}{}
	}
	for name := range desiredByName {
		nameSet[name] = struct{}{}
	}

	orderedNames := make([]string, 0, len(nameSet))
	for name := range nameSet {
		orderedNames = append(orderedNames, name)
	}
	sort.Strings(orderedNames)

	for _, name := range orderedNames {
		existingGroup, hasExisting := existingByName[name]
		desiredGroup, hasDesired := desiredByName[name]

		switch {
		case hasExisting && !hasDesired:
			logger.Info("Deleting obsolete loxilb rule group", "name", name)
			if err := r.LoxiClient.LoadBalancer().DeleteByName(ctx, name); err != nil {
				logger.Error(err, "failed to delete obsolete rule group", "name", name)
				return err
			}

		case !hasExisting && hasDesired:
			logger.Info("Creating new loxilb rule group", "name", name)
			if err := r.createRuleGroup(ctx, desiredGroup, false); err != nil {
				logger.Error(err, "failed to create new rule group", "name", name)
				return err
			}

		case hasExisting && hasDesired:
			if !ruleGroupChanged(existingGroup, desiredGroup) {
				continue
			}

			logger.Info("Updating loxilb rule group (delete+recreate)", "name", name)
			if err := r.LoxiClient.LoadBalancer().DeleteByName(ctx, name); err != nil {
				logger.Error(err, "failed to delete rule group for update", "name", name)
				return err
			}

			if err := r.createRuleGroup(ctx, desiredGroup, true); err != nil {
				logger.Error(err, "failed to recreate rule group after update", "name", name)
				return err
			}
		}
	}

	return nil
}

func (r *LoxilbIngressReconciler) createDirectLoxiLoadBalancerService(ns, name, externalIP, protocol, host, epSelect string, port int32) loxiapi.LoadBalancerService {
	service := loxiapi.LoadBalancerService{
		ExternalIP: externalIP,
		Protocol:   strings.ToLower(protocol),
		Mode:       4, // fullproxy mode
		Name:       fmt.Sprintf("%s_%s", ns, name),
		Host:       host,
		Port:       uint16(port),
	}

	switch epSelect {
	case pkg.EndPointSel_RR:
		service.Sel = loxiapi.LbSelRr
	case pkg.EndPointSel_HASH:
		service.Sel = loxiapi.LbSelHash
	case pkg.EndpointSel_PRIORITY:
		service.Sel = loxiapi.LbSelPrio
	case pkg.EndPointSel_PERSIST:
		service.Sel = loxiapi.LbSelRrPersist
	case pkg.EndPointSel_LC:
		service.Sel = loxiapi.LbSelLeastConnections
	case pkg.EndPointSel_N2:
		service.Sel = loxiapi.LbSelN2
	default:
		service.Sel = loxiapi.LbSelRr
	}

	return service
}

func (r *LoxilbIngressReconciler) createLoxiLoadBalancerService(ns, name, externalIP, epSelect string, security int32, host, path, pathType string, mtlsFrontend *loxiapi.MtlsFrontend, mtlsBackend *loxiapi.MtlsBackend) loxiapi.LoadBalancerService {
	service := loxiapi.LoadBalancerService{
		ExternalIP:      externalIP,
		Protocol:        "tcp",
		Mode:            4, // fullproxy mode
		Name:            fmt.Sprintf("%s_%s", ns, name),
		Host:            host,
		PathPrefix:      path,
		PathMatchMode:   loxiapi.PathMatchModeType(strings.ToLower(pathType)),
		BackendProtocol: "http1",
		Security:        security,
		MtlsFrontend:    mtlsFrontend,
		MtlsBackend:     mtlsBackend,
	}

	switch epSelect {
	case pkg.EndPointSel_RR:
		service.Sel = loxiapi.LbSelRr
	case pkg.EndPointSel_HASH:
		service.Sel = loxiapi.LbSelHash
	case pkg.EndpointSel_PRIORITY:
		service.Sel = loxiapi.LbSelPrio
	case pkg.EndPointSel_PERSIST:
		service.Sel = loxiapi.LbSelRrPersist
	case pkg.EndPointSel_LC:
		service.Sel = loxiapi.LbSelLeastConnections
	case pkg.EndPointSel_N2:
		service.Sel = loxiapi.LbSelN2
	default:
		service.Sel = loxiapi.LbSelRr
	}

	// when ingress is set TLS, using https port (443)
	if security == 0 {
		service.Port = 80
	} else {
		service.Port = 443
	}

	return service
}

func (r *LoxilbIngressReconciler) createLoxiLoadBalancerEndpoints(ctx context.Context, ns, name string) ([]loxiapi.LoadBalancerEndpoint, error) {
	loxilbEpList := make([]loxiapi.LoadBalancerEndpoint, 0)

	// List EndpointSlices for the service
	epSliceList := &discoveryv1.EndpointSliceList{}
	listOpts := []client.ListOption{
		client.InNamespace(ns),
		client.MatchingLabels{
			"kubernetes.io/service-name": name,
		},
	}

	if err := r.List(ctx, epSliceList, listOpts...); err != nil {
		return loxilbEpList, err
	}

	// Iterate through all EndpointSlices
	for _, epSlice := range epSliceList.Items {
		for _, endpoint := range epSlice.Endpoints {
			// Skip endpoints that are not ready
			if endpoint.Conditions.Ready != nil && !*endpoint.Conditions.Ready {
				continue
			}

			// Get endpoint addresses
			for _, addr := range endpoint.Addresses {
				// Get ports from EndpointSlice
				for _, port := range epSlice.Ports {
					if port.Port != nil {
						loxilbEp := loxiapi.LoadBalancerEndpoint{
							EndpointIP: addr,
							TargetPort: uint16(*port.Port),
							Weight:     uint8(1),
						}
						loxilbEpList = append(loxilbEpList, loxilbEp)
					}
				}
			}
		}
	}

	if len(loxilbEpList) <= 0 {
		return loxilbEpList, fmt.Errorf("no endpoints have been added to the %s/%s service yet. please wait", ns, name)
	}

	return loxilbEpList, nil
}

func (r *LoxilbIngressReconciler) createLoxiLoadBalancerEndpointsWithTargetPort(ctx context.Context, ns, name string, targetPort int32) ([]loxiapi.LoadBalancerEndpoint, error) {
	loxilbEpList := make([]loxiapi.LoadBalancerEndpoint, 0)

	// List EndpointSlices for the service
	epSliceList := &discoveryv1.EndpointSliceList{}
	listOpts := []client.ListOption{
		client.InNamespace(ns),
		client.MatchingLabels{
			"kubernetes.io/service-name": name,
		},
	}

	if err := r.List(ctx, epSliceList, listOpts...); err != nil {
		return loxilbEpList, err
	}

	// Iterate through all EndpointSlices
	for _, epSlice := range epSliceList.Items {
		for _, endpoint := range epSlice.Endpoints {
			// Skip endpoints that are not ready
			if endpoint.Conditions.Ready != nil && !*endpoint.Conditions.Ready {
				continue
			}

			// Get endpoint addresses
			for _, addr := range endpoint.Addresses {
				loxilbEp := loxiapi.LoadBalancerEndpoint{
					EndpointIP: addr,
					TargetPort: uint16(targetPort),
					Weight:     uint8(1),
				}
				loxilbEpList = append(loxilbEpList, loxilbEp)
			}
		}
	}

	if len(loxilbEpList) <= 0 {
		return loxilbEpList, fmt.Errorf("no endpoints have been added to the %s/%s service yet. please wait", ns, name)
	}

	return loxilbEpList, nil
}

func (r *LoxilbIngressReconciler) checkTLSHost(host string, TLS []netv1.IngressTLS) bool {
	for _, tls := range TLS {
		for _, tlsHost := range tls.Hosts {
			if host == tlsHost {
				return true
			}
		}
	}
	return false
}

// getMtlsFrontendConfig reads mTLS frontend configuration from Ingress annotations and K8s secrets
func (r *LoxilbIngressReconciler) getMtlsFrontendConfig(ctx context.Context, ingress *netv1.Ingress) (*loxiapi.MtlsFrontend, error) {
	mode, ok := ingress.Annotations[mtlsFrontendModeAnnotation]
	if !ok || mode == "" || mode == "disabled" {
		return nil, nil
	}

	if mode != "optional" && mode != "required" {
		return nil, fmt.Errorf("invalid mtls-frontend-mode: %s (must be 'disabled', 'optional', or 'required')", mode)
	}

	mtlsFrontend := &loxiapi.MtlsFrontend{
		ClientCertMode: &mode,
	}

	// Get client CA certificate from secret
	secretName, ok := ingress.Annotations[mtlsFrontendSecretAnnotation]
	if !ok || secretName == "" {
		return nil, fmt.Errorf("mtls-frontend-secret is required when mtls-frontend-mode is '%s'", mode)
	}

	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: ingress.Namespace, Name: secretName}, secret); err != nil {
		return nil, fmt.Errorf("failed to get secret %s/%s: %w", ingress.Namespace, secretName, err)
	}

	// Try client-ca.crt first, fallback to server.crt
	clientCACert, ok := secret.Data["client-ca.crt"]
	if !ok || len(clientCACert) == 0 {
		clientCACert, ok = secret.Data["server.crt"]
		if !ok || len(clientCACert) == 0 {
			return nil, fmt.Errorf("secret %s/%s does not contain 'client-ca.crt' or 'server.crt' key", ingress.Namespace, secretName)
		}
	}

	// Store certificate to filesystem and get the path
	certPath, err := r.CertManager.StoreMtlsFrontendCert(ctx, ingress.Namespace, ingress.Name, clientCACert)
	if err != nil {
		return nil, fmt.Errorf("failed to store frontend CA certificate: %w", err)
	}

	// Set path instead of base64 data
	mtlsFrontend.ClientCaPath = certPath

	// Check CN verification
	if requireCn, ok := ingress.Annotations[mtlsFrontendRequireCnAnnotation]; ok && requireCn == "true" {
		requireCnBool := true
		mtlsFrontend.RequireClientCn = &requireCnBool

		cnPattern, ok := ingress.Annotations[mtlsFrontendCnPatternAnnotation]
		if !ok || cnPattern == "" {
			return nil, fmt.Errorf("mtls-frontend-cn-pattern is required when mtls-frontend-require-cn is true")
		}
		mtlsFrontend.ClientCnPattern = cnPattern
	}

	return mtlsFrontend, nil
}

// getMtlsBackendConfig reads mTLS backend configuration from Ingress annotations and K8s secrets
func (r *LoxilbIngressReconciler) getMtlsBackendConfig(ctx context.Context, ingress *netv1.Ingress) (*loxiapi.MtlsBackend, error) {
	verify, ok := ingress.Annotations[mtlsBackendVerifyAnnotation]
	if !ok || verify != "true" {
		return nil, nil
	}

	verifyBool := true
	mtlsBackend := &loxiapi.MtlsBackend{
		VerifyServerCert: &verifyBool,
	}

	var caCertData []byte
	var clientCertData []byte
	var clientKeyData []byte

	// Get backend CA certificate from secret (optional but recommended)
	if caSecretName, ok := ingress.Annotations[mtlsBackendCaSecretAnnotation]; ok && caSecretName != "" {
		secret := &corev1.Secret{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: ingress.Namespace, Name: caSecretName}, secret); err != nil {
			return nil, fmt.Errorf("failed to get backend CA secret %s/%s: %w", ingress.Namespace, caSecretName, err)
		}

		backendCACert, ok := secret.Data["backend-ca.crt"]
		if !ok || len(backendCACert) == 0 {
			backendCACert, ok = secret.Data["server.crt"]
			if !ok || len(backendCACert) == 0 {
				return nil, fmt.Errorf("secret %s/%s does not contain 'backend-ca.crt' or 'server.crt' key", ingress.Namespace, caSecretName)
			}
		}
		caCertData = backendCACert
	}

	// Get LoxiLB client certificate and key from secret
	if clientSecretName, ok := ingress.Annotations[mtlsBackendClientSecretAnnotation]; ok && clientSecretName != "" {
		secret := &corev1.Secret{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: ingress.Namespace, Name: clientSecretName}, secret); err != nil {
			return nil, fmt.Errorf("failed to get backend client secret %s/%s: %w", ingress.Namespace, clientSecretName, err)
		}

		// Try tls.crt first, fallback to server.crt
		clientCert, ok := secret.Data["tls.crt"]
		if !ok || len(clientCert) == 0 {
			clientCert, ok = secret.Data["server.crt"]
			if !ok || len(clientCert) == 0 {
				return nil, fmt.Errorf("secret %s/%s does not contain 'tls.crt' or 'server.crt' key", ingress.Namespace, clientSecretName)
			}
		}

		// Try tls.key first, fallback to server.key
		clientKey, ok := secret.Data["tls.key"]
		if !ok || len(clientKey) == 0 {
			clientKey, ok = secret.Data["server.key"]
			if !ok || len(clientKey) == 0 {
				return nil, fmt.Errorf("secret %s/%s does not contain 'tls.key' or 'server.key' key", ingress.Namespace, clientSecretName)
			}
		}

		clientCertData = clientCert
		clientKeyData = clientKey
	}

	// Store certificates to filesystem and get the paths
	if len(clientCertData) > 0 || len(clientKeyData) > 0 || len(caCertData) > 0 {
		caPath, certPath, keyPath, err := r.CertManager.StoreMtlsBackendCerts(ctx, ingress.Namespace, ingress.Name, caCertData, clientCertData, clientKeyData)
		if err != nil {
			return nil, fmt.Errorf("failed to store backend certificates: %w", err)
		}

		// Set paths instead of base64 data
		if caPath != "" {
			mtlsBackend.BackendCaPath = caPath
		}
		if certPath != "" {
			mtlsBackend.ClientCertPath = certPath
		}
		if keyPath != "" {
			mtlsBackend.ClientKeyPath = keyPath
		}
	}

	return mtlsBackend, nil
}

func (r *LoxilbIngressReconciler) getBackendServiceNamespace(ingress *netv1.Ingress, backendName string) string {
	if _, isok := ingress.Annotations["external-backend-service"]; isok {
		if backendNamespace, isNs := ingress.Annotations["service-"+backendName+"-namespace"]; isNs {
			return backendNamespace
		}
	}
	return ingress.Namespace
}

func (r *LoxilbIngressReconciler) createDirectLoxiModelList(ctx context.Context, ingress *netv1.Ingress) ([]loxiapi.LoadBalancerModel, error) {
	svcName, isSvc := ingress.Annotations["loxilb.io/direct-loadbalance-service"]
	if !isSvc {
		return nil, fmt.Errorf("no service name is set in the ingress annotation for direct-loadbalance")
	}

	svcNs, isNs := ingress.Annotations["loxilb.io/direct-loadbalance-namespace"]
	if !isNs {
		svcNs = ingress.Namespace
	}

	selStr, isSel := ingress.Annotations["loxilb.io/epselect"]
	if !isSel {
		selStr = pkg.EndPointSel_RR
	}

	svc := &corev1.Service{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: svcNs, Name: svcName}, svc); err != nil {
		return nil, err
	}

	models := make([]loxiapi.LoadBalancerModel, 0)
	lbName := ingress.Name
	for _, port := range svc.Spec.Ports {
		protocol := string(port.Protocol)
		targetPortNum, err := r.GetServicePortIntValue(svc, port)
		if err != nil {
			return models, err
		}
		loxisvc := r.createDirectLoxiLoadBalancerService(ingress.Namespace, lbName, "0.0.0.0", protocol, "", selStr, port.Port)
		loxiep, err := r.createLoxiLoadBalancerEndpointsWithTargetPort(ctx, svcNs, svcName, targetPortNum)
		if err != nil {
			return models, err
		}

		model := loxiapi.LoadBalancerModel{
			Service:   loxisvc,
			Endpoints: loxiep,
		}
		models = append(models, model)

	}

	return models, nil
}

func (r *LoxilbIngressReconciler) createLoxiModelList(ctx context.Context, ingress *netv1.Ingress) ([]loxiapi.LoadBalancerModel, error) {
	models := make([]loxiapi.LoadBalancerModel, 0)
	selStr, isSel := ingress.Annotations["loxilb.io/epselect"]
	if !isSel {
		selStr = pkg.EndPointSel_RR
	}

	// Read mTLS configuration from Ingress annotations
	mtlsFrontend, err := r.getMtlsFrontendConfig(ctx, ingress)
	if err != nil {
		return nil, fmt.Errorf("failed to get mTLS frontend config: %w", err)
	}

	mtlsBackend, err := r.getMtlsBackendConfig(ctx, ingress)
	if err != nil {
		return nil, fmt.Errorf("failed to get mTLS backend config: %w", err)
	}

	for _, rule := range ingress.Spec.Rules {
		if rule.HTTP == nil {
			continue
		}

		for _, path := range rule.HTTP.Paths {
			if path.Backend.Service != nil {
				name := path.Backend.Service.Name
				ns := r.getBackendServiceNamespace(ingress, name)
				port := path.Backend.Service.Port.Number
				security := int32(0)

				// Determine security level based on TLS/mTLS configuration
				if mtlsBackend != nil {
					security = 2 // E2E HTTPS with backend mTLS
				} else if mtlsFrontend != nil {
					security = 1 // Frontend TLS with client cert verification
				} else if r.checkTLSHost(rule.Host, ingress.Spec.TLS) {
					security = 1 // Standard TLS from spec
				}

				lbName := ingress.Name
				if security >= 1 {
					lbName += "_https"
				}

				// Get pathType, default to "prefix" if not specified
				pathType := "prefix"
				if path.PathType != nil {
					pathType = string(*path.PathType)
				}

				loxisvc := r.createLoxiLoadBalancerService(ingress.Namespace, lbName, r.LoxiClient.Host, selStr, security, rule.Host, path.Path, pathType, mtlsFrontend, mtlsBackend)
				loxiep, err := r.createLoxiLoadBalancerEndpointsWithTargetPort(ctx, ns, name, port)
				if err != nil {
					return models, err
				}

				model := loxiapi.LoadBalancerModel{
					Service:   loxisvc,
					Endpoints: loxiep,
				}
				models = append(models, model)
			}
		}
	}

	return models, nil
}

func (r *LoxilbIngressReconciler) updateIngressStatus(ctx context.Context, ingress *netv1.Ingress) error {
	lbSvcKey := types.NamespacedName{}
	if gwProvider, isok := ingress.Annotations["gateway-api-controller"]; isok {
		if gwProvider == "loxilb.io/loxilb" {
			lbSvcKey.Namespace = ingress.Annotations["parent-gateway-namespace"]
			lbSvcKey.Name = fmt.Sprintf("%s-ingress-service", ingress.Annotations["parent-gateway"])
		}
	} else {
		if lbNs, isok := ingress.Annotations["loadbalancer-service-namespace"]; isok {
			lbSvcKey.Namespace = lbNs
		} else {
			lbSvcKey.Namespace = "default"
		}

		if lbName, isok := ingress.Annotations["loadbalancer-service"]; isok {
			lbSvcKey.Name = lbName
		} else {
			return nil
		}
	}

	svc := &corev1.Service{}
	if err := r.Get(ctx, lbSvcKey, svc); err != nil {
		return err
	}

	for _, ing := range svc.Status.LoadBalancer.Ingress {
		if r.checkIngressLoadBalancerIngressExist(ingress, ing) {
			continue
		}

		newIngressLoadBalancerIngress := netv1.IngressLoadBalancerIngress{
			IP:       ing.IP,
			Hostname: ing.Hostname,
		}
		for _, port := range ing.Ports {
			newIngressPortStatus := netv1.IngressPortStatus{
				Port:     port.Port,
				Protocol: port.Protocol,
				Error:    port.Error,
			}
			newIngressLoadBalancerIngress.Ports = append(newIngressLoadBalancerIngress.Ports, newIngressPortStatus)
		}

		ingress.Status.LoadBalancer.Ingress = append(ingress.Status.LoadBalancer.Ingress, newIngressLoadBalancerIngress)
	}

	return r.Status().Update(ctx, ingress)
}

func (r *LoxilbIngressReconciler) checkIngressLoadBalancerIngressExist(ingress *netv1.Ingress, serviceIngress corev1.LoadBalancerIngress) bool {
	for _, i := range ingress.Status.LoadBalancer.Ingress {
		if i.IP != "" {
			if i.IP == serviceIngress.IP {
				return true
			}
		}
		if i.Hostname != "" {
			if i.Hostname == serviceIngress.Hostname {
				return true
			}
		}
	}

	return false
}

func (r *LoxilbIngressReconciler) GetServicePortIntValue(svc *corev1.Service, port corev1.ServicePort) (int32, error) {
	if port.TargetPort.IntValue() != 0 {
		return int32(port.TargetPort.IntValue()), nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()

	selectorLabel := labels.Set(svc.Spec.Selector).AsSelector()
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList, client.MatchingLabelsSelector{Selector: selectorLabel}); err != nil {
		return 0, err
	}

	for _, pod := range podList.Items {
		for _, c := range pod.Spec.Containers {
			for _, p := range c.Ports {
				if p.Name == port.TargetPort.String() {
					return p.ContainerPort, nil
				}
			}
		}
	}

	return 0, fmt.Errorf("not found port name %s in service %s", port.TargetPort.String(), svc.Name)
}

func (r *LoxilbIngressReconciler) SetupWithManager(mgr ctrl.Manager) error {
	checkIngClassNameFunc := isLoxilbIngress

	// secretToIngressMapper maps Secret changes to related Ingress objects
	secretToIngressMapper := func(ctx context.Context, obj client.Object) []reconcile.Request {
		secret := obj.(*corev1.Secret)

		// List all Ingresses in the same namespace
		ingressList := &netv1.IngressList{}
		if err := r.List(ctx, ingressList, client.InNamespace(secret.Namespace)); err != nil {
			return []reconcile.Request{}
		}

		// Find Ingresses that reference this Secret
		requests := []reconcile.Request{}
		for _, ing := range ingressList.Items {
			if !checkIngClassNameFunc(&ing) {
				continue
			}

			shouldReconcile := false

			// Check if this Ingress references the Secret in its TLS configuration
			for _, tls := range ing.Spec.TLS {
				if tls.SecretName == secret.Name {
					shouldReconcile = true
					break
				}
			}

			// Check if this Ingress references the Secret in mTLS annotations
			if !shouldReconcile {
				if frontendSecret, ok := ing.Annotations[mtlsFrontendSecretAnnotation]; ok && frontendSecret == secret.Name {
					shouldReconcile = true
				}
			}

			if !shouldReconcile {
				if backendCASecret, ok := ing.Annotations[mtlsBackendCaSecretAnnotation]; ok && backendCASecret == secret.Name {
					shouldReconcile = true
				}
			}

			if !shouldReconcile {
				if backendClientSecret, ok := ing.Annotations[mtlsBackendClientSecretAnnotation]; ok && backendClientSecret == secret.Name {
					shouldReconcile = true
				}
			}

			if shouldReconcile {
				requests = append(requests, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Namespace: ing.Namespace,
						Name:      ing.Name,
					},
				})
			}
		}

		return requests
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&netv1.Ingress{}).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(secretToIngressMapper)).
		WithEventFilter(predicate.Funcs{
			UpdateFunc: func(e event.UpdateEvent) bool {
				if ingNew, ok := e.ObjectNew.(*netv1.Ingress); ok {
					if checkIngClassNameFunc(ingNew) {
						return true
					}
				}

				if ingOld, ok := e.ObjectOld.(*netv1.Ingress); ok {
					if checkIngClassNameFunc(ingOld) {
						return true
					}
				}

				if _, ok := e.ObjectNew.(*corev1.Secret); ok {
					return true
				}

				return false
			},
			DeleteFunc: func(e event.DeleteEvent) bool {
				ing, ok := e.Object.(*netv1.Ingress)
				if ok {
					return checkIngClassNameFunc(ing)
				}
				if _, ok := e.Object.(*corev1.Secret); ok {
					return true
				}
				return false
			},
			CreateFunc: func(e event.CreateEvent) bool {
				ing, ok := e.Object.(*netv1.Ingress)
				if ok {
					return checkIngClassNameFunc(ing)
				}
				if _, ok := e.Object.(*corev1.Secret); ok {
					return true
				}
				return false
			},
			GenericFunc: func(e event.GenericEvent) bool {
				ing, ok := e.Object.(*netv1.Ingress)
				if ok {
					return checkIngClassNameFunc(ing)
				}
				if _, ok := e.Object.(*corev1.Secret); ok {
					return true
				}
				return false
			},
		}).
		Complete(r)
}
