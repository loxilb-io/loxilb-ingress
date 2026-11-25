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
	"strconv"
	"strings"
	"time"

	loxiapi "github.com/loxilb-io/kube-loxilb/pkg/api"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	netv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
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

	// backendRetryInterval is how soon an Ingress with backends that couldn't be installed
	// (for example, no endpoints yet) is reconciled again.
	backendRetryInterval = 5 * time.Second
	// resyncInterval is how often an Ingress is reconciled when nothing asks for it sooner.
	resyncInterval = 30 * time.Second

	// defaultInactiveTimeout is the inactivity timeout, in seconds, of the HTTP and HTTPS rules of
	// an Ingress without the loxilb.io/timeout annotation. It is the default kube-loxilb gives its
	// own rules. loxilb's default of 240 closes a quiet WebSocket after four minutes.
	defaultInactiveTimeout = 30 * 60
	// maxInactiveTimeout is the longest inactivity timeout loxilb accepts.
	maxInactiveTimeout = 24 * 60 * 60

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
	// Recorder writes the events of an Ingress. Nil disables them.
	Recorder record.EventRecorder
	// ProgrammedKey is the annotation key under which this Pod reports the Ingress generation it
	// has installed (see programmed.go). Empty disables the report.
	ProgrammedKey string

	// initialSync tracks the first reconcile pass that readiness waits for (see readiness.go).
	initialSync initialSync
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
	// loxilb answered, so this attempt counts toward the first reconcile pass that readiness waits for.
	defer r.initialSync.reconciled(req.NamespacedName)

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
		r.clearProgrammedOrLog(ctx, ingress)
		return r.handleIngressDeleted(ctx, req.Namespace, req.Name, existingRules)
	}

	// Process TLS certificates before creating loxilb rules
	if r.CertManager != nil && len(ingress.Spec.TLS) > 0 {
		providers, err := r.tlsHostProviders(ctx)
		if err != nil {
			logger.Error(err, "failed to process TLS certificates", "ingress", ingress.Name)
			return ctrl.Result{}, err
		}
		providesHost := func(host string) bool { return providers[host] == req.NamespacedName }
		if err := r.CertManager.ProcessIngressTLS(ctx, ingress, providesHost); err != nil {
			logger.Error(err, "failed to process TLS certificates", "ingress", ingress.Name)
			return ctrl.Result{}, err
		}
	}

	// when ingress is added, install rule to loxilb-ingress
	var models []loxiapi.LoadBalancerModel
	var skipped []error
	if _, isok := ingress.Annotations["loxilb.io/direct-loadbalance-service"]; isok {
		models, skipped, err = r.createDirectLoxiModelList(ctx, ingress)
	} else {
		// Nothing is installed yet, so a failure here leaves the rules as they are.
		owners, ownersErr := r.rulePathOwners(ctx)
		if ownersErr != nil {
			logger.Error(ownersErr, "failed to set ingress", "Ingress", req.NamespacedName)
			return ctrl.Result{}, ownersErr
		}
		models, skipped, err = r.createLoxiModelList(ctx, ingress, owners)
	}

	if err != nil {
		if cleanupErr := r.deleteManagedRules(ctx, existingRules); cleanupErr != nil {
			logger.Error(cleanupErr, "failed to cleanup existing loxilb-ingress rules after model generation error")
		}
		r.clearProgrammedOrLog(ctx, ingress)
		logger.Error(err, "Failed to set ingress. failed to create loxilb loadbalancer model", "[]loxiapi.LoadBalancerModel", models)
		return ctrl.Result{}, err
	}

	// Paths whose backends can't be resolved yet are left out; the rest are installed.
	if err := r.syncRules(ctx, existingRules, models); err != nil {
		r.clearProgrammedOrLog(ctx, ingress)
		logger.Error(err, "failed to synchronize loxilb-ingress rules", "Ingress", req.NamespacedName)
		return ctrl.Result{}, err
	}

	// A path left out for an older Ingress isn't retried: the Ingress is reconciled again when
	// that Ingress changes. Only backends that aren't ready yet are.
	conflicts, notReady := splitPathConflicts(skipped)
	r.reportPathConflicts(ctx, ingress, conflicts)
	retryAfter := resyncInterval
	if len(notReady) > 0 {
		retryAfter = backendRetryInterval
	}

	if len(models) == 0 && len(skipped) > 0 {
		// Nothing could be installed, so this Pod doesn't serve the Ingress at all.
		r.clearProgrammedOrLog(ctx, ingress)
		logger.Error(utilerrors.NewAggregate(skipped), "no Ingress path could be installed", "Ingress", req.NamespacedName)
		return ctrl.Result{RequeueAfter: retryAfter}, nil
	}

	if err := r.updateIngressStatus(ctx, ingress); err != nil {
		logger.Info("failed to update ingress status.", "error", err)
	}

	// Report after the status update: the patch changes the resourceVersion that the status
	// update relies on. The report covers what was installed; skipped paths can't be served by any
	// route until their backends are ready. A path that an older Ingress serves is different: it
	// answers with another Ingress's backend, so the Ingress isn't reported.
	if len(conflicts) > 0 {
		r.clearProgrammedOrLog(ctx, ingress)
	} else if err := r.markProgrammed(ctx, ingress); err != nil {
		logger.Error(err, "failed to report programmed generation", "Ingress", req.NamespacedName, "key", r.ProgrammedKey)
		return ctrl.Result{}, err
	}

	if len(skipped) > 0 {
		logger.Error(utilerrors.NewAggregate(skipped), "some Ingress paths are not installed",
			"Ingress", req.NamespacedName, "installed", len(models), "skipped", len(skipped))
		return ctrl.Result{RequeueAfter: retryAfter}, nil
	}

	logger.Info("Ingress reconciled successfully", "ingress", req.NamespacedName)
	return ctrl.Result{RequeueAfter: resyncInterval}, nil
}

func (r *LoxilbIngressReconciler) clearProgrammedOrLog(ctx context.Context, ingress *netv1.Ingress) {
	if err := r.clearProgrammed(ctx, ingress); err != nil {
		log.FromContext(ctx).Error(err, "failed to clear programmed report", "Ingress", client.ObjectKeyFromObject(ingress), "key", r.ProgrammedKey)
	}
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
		// A host that another Ingress still lists keeps its certificate.
		if providers, err := r.tlsHostProviders(ctx); err != nil {
			logger.Error(err, "failed to cleanup TLS certificates")
		} else {
			unusedHosts := make([]string, 0)
			for _, host := range uniqueHTTPSHosts(existingRules) {
				if _, used := providers[host]; !used {
					unusedHosts = append(unusedHosts, host)
				}
			}
			if err := r.CertManager.CleanupIngressCertificates(ctx, unusedHosts); err != nil {
				logger.Error(err, "failed to cleanup TLS certificates")
			}
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
	// Rules installed without a timeout (direct-loadbalance) run with loxilb's default.
	if desired.Timeout != 0 && existing.Timeout != desired.Timeout {
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

// createLoxiLoadBalancerEndpointsForBackend builds the endpoints for an Ingress service backend.
// The Ingress refers to a Service port, but traffic has to reach the Pod-side port, so the
// referenced ServicePort is looked up on the Service and its resolved port is read from each
// EndpointSlice.
func (r *LoxilbIngressReconciler) createLoxiLoadBalancerEndpointsForBackend(ctx context.Context, ns string, backend *netv1.IngressServiceBackend) ([]loxiapi.LoadBalancerEndpoint, error) {
	svc := &corev1.Service{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: backend.Name}, svc); err != nil {
		return nil, fmt.Errorf("failed to get backend service %s/%s: %w", ns, backend.Name, err)
	}

	svcPort, err := findServicePort(svc, backend.Port)
	if err != nil {
		return nil, err
	}

	return r.createLoxiLoadBalancerEndpointsForServicePort(ctx, svc, svcPort)
}

// createLoxiLoadBalancerEndpointsForServicePort builds the endpoints for one port of a Service,
// using the Pod-side port that each EndpointSlice publishes for that ServicePort.
func (r *LoxilbIngressReconciler) createLoxiLoadBalancerEndpointsForServicePort(ctx context.Context, svc *corev1.Service, svcPort corev1.ServicePort) ([]loxiapi.LoadBalancerEndpoint, error) {
	singleUnnamedPort := len(svc.Spec.Ports) == 1 && svcPort.Name == ""

	epSliceList := &discoveryv1.EndpointSliceList{}
	listOpts := []client.ListOption{
		client.InNamespace(svc.Namespace),
		client.MatchingLabels{discoveryv1.LabelServiceName: svc.Name},
	}
	if err := r.List(ctx, epSliceList, listOpts...); err != nil {
		return nil, err
	}

	loxilbEpList := make([]loxiapi.LoadBalancerEndpoint, 0)
	for i := range epSliceList.Items {
		epSlice := &epSliceList.Items[i]
		// Resolve per slice: Pods behind a named targetPort can expose different port numbers,
		// and the EndpointSlice controller puts them in separate slices.
		targetPort, ok := endpointSliceTargetPort(epSlice, svcPort, singleUnnamedPort)
		if !ok {
			continue
		}

		for _, endpoint := range epSlice.Endpoints {
			// Skip endpoints that are not ready
			if endpoint.Conditions.Ready != nil && !*endpoint.Conditions.Ready {
				continue
			}

			for _, addr := range endpoint.Addresses {
				loxilbEpList = append(loxilbEpList, loxiapi.LoadBalancerEndpoint{
					EndpointIP: addr,
					TargetPort: uint16(targetPort),
					Weight:     uint8(1),
				})
			}
		}
	}

	if len(loxilbEpList) == 0 {
		return loxilbEpList, fmt.Errorf("no endpoints have been added to the %s/%s service port %d/%s yet. please wait", svc.Namespace, svc.Name, svcPort.Port, servicePortProtocol(svcPort))
	}

	return loxilbEpList, nil
}

// findServicePort returns the TCP ServicePort that an Ingress backend port refers to.
// An Ingress backend selects a Service port by its name or by its port number, never by targetPort.
func findServicePort(svc *corev1.Service, backendPort netv1.ServiceBackendPort) (corev1.ServicePort, error) {
	for _, sp := range svc.Spec.Ports {
		if sp.Protocol != "" && sp.Protocol != corev1.ProtocolTCP {
			continue
		}
		if backendPort.Name != "" {
			if sp.Name == backendPort.Name {
				return sp, nil
			}
		} else if sp.Port == backendPort.Number {
			return sp, nil
		}
	}
	return corev1.ServicePort{}, fmt.Errorf("service %s/%s has no TCP port %s", svc.Namespace, svc.Name, describeBackendPort(backendPort))
}

// endpointSliceTargetPort returns the Pod-side port an EndpointSlice publishes for svcPort.
// EndpointSlice ports carry the ServicePort's name and the already resolved targetPort, both for
// controller-managed slices and for hand-written ones of selector-less Services.
// For a Service with a single unnamed port, a slice with a single port of the same protocol is
// accepted whatever its name.
func endpointSliceTargetPort(epSlice *discoveryv1.EndpointSlice, svcPort corev1.ServicePort, singleUnnamedPort bool) (int32, bool) {
	protocol := servicePortProtocol(svcPort)
	candidates := make([]int32, 0, len(epSlice.Ports))
	for _, p := range epSlice.Ports {
		if p.Port == nil {
			continue
		}
		// EndpointPort.Protocol defaults to TCP, like ServicePort.Protocol.
		epProtocol := corev1.ProtocolTCP
		if p.Protocol != nil {
			epProtocol = *p.Protocol
		}
		if epProtocol != protocol {
			continue
		}
		name := ""
		if p.Name != nil {
			name = *p.Name
		}
		if name == svcPort.Name {
			return *p.Port, true
		}
		candidates = append(candidates, *p.Port)
	}

	if singleUnnamedPort && len(candidates) == 1 {
		return candidates[0], true
	}
	return 0, false
}

func servicePortProtocol(sp corev1.ServicePort) corev1.Protocol {
	if sp.Protocol == "" {
		return corev1.ProtocolTCP
	}
	return sp.Protocol
}

func describeBackendPort(p netv1.ServiceBackendPort) string {
	if p.Name != "" {
		return fmt.Sprintf("%q", p.Name)
	}
	return fmt.Sprintf("%d", p.Number)
}

// inactiveTimeout returns the inactivity timeout, in seconds, for the HTTP and HTTPS rules of the
// Ingress: the loxilb.io/timeout annotation, or defaultInactiveTimeout without it. A value that
// isn't a positive number of seconds is ignored, and one above loxilb's limit is lowered to it,
// so that a bad annotation doesn't keep the rules from being installed.
func inactiveTimeout(ctx context.Context, ingress *netv1.Ingress) uint32 {
	value, ok := ingress.Annotations[pkg.TimeoutAnnotation]
	if !ok {
		return defaultInactiveTimeout
	}

	logger := log.FromContext(ctx)
	seconds, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || seconds <= 0 {
		logger.Info("ignoring invalid timeout annotation", "annotation", pkg.TimeoutAnnotation, "value", value, "using", defaultInactiveTimeout)
		return defaultInactiveTimeout
	}
	if seconds > maxInactiveTimeout {
		logger.Info("timeout annotation is above the limit", "annotation", pkg.TimeoutAnnotation, "value", value, "using", maxInactiveTimeout)
		return maxInactiveTimeout
	}
	return uint32(seconds)
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
	if _, isok := ingress.Annotations[pkg.ExternalBackendServiceAnnotation]; isok {
		if backendNamespace, isNs := ingress.Annotations["service-"+backendName+"-namespace"]; isNs {
			return backendNamespace
		}
	}
	return ingress.Namespace
}

// createDirectLoxiModelList builds one rule per port of the direct-loadbalance Service. Ports that
// have no endpoints yet are left out and returned in skipped.
func (r *LoxilbIngressReconciler) createDirectLoxiModelList(ctx context.Context, ingress *netv1.Ingress) (models []loxiapi.LoadBalancerModel, skipped []error, err error) {
	svcName, isSvc := ingress.Annotations["loxilb.io/direct-loadbalance-service"]
	if !isSvc {
		return nil, nil, fmt.Errorf("no service name is set in the ingress annotation for direct-loadbalance")
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
		return nil, []error{fmt.Errorf("failed to get direct-loadbalance service %s/%s: %w", svcNs, svcName, err)}, nil
	}

	models = make([]loxiapi.LoadBalancerModel, 0)
	lbName := ingress.Name
	for _, port := range svc.Spec.Ports {
		protocol := string(port.Protocol)
		loxisvc := r.createDirectLoxiLoadBalancerService(ingress.Namespace, lbName, "0.0.0.0", protocol, "", selStr, port.Port)
		loxiep, err := r.createLoxiLoadBalancerEndpointsForServicePort(ctx, svc, port)
		if err != nil {
			skipped = append(skipped, err)
			continue
		}

		model := loxiapi.LoadBalancerModel{
			Service:   loxisvc,
			Endpoints: loxiep,
		}
		models = append(models, model)
	}

	return models, skipped, nil
}

// createLoxiModelList builds one rule per Ingress path. A path whose backend can't be resolved yet
// (no Service, no such port, or no endpoints) is left out and returned in skipped, so that one
// failing backend doesn't take down the other paths. So is a path whose rule goes to another
// Ingress according to owners (see pathowners.go); a nil owners leaves no path out for that.
// err is set only for errors that affect the whole Ingress, such as invalid mTLS settings.
func (r *LoxilbIngressReconciler) createLoxiModelList(ctx context.Context, ingress *netv1.Ingress, owners map[rulePath]types.NamespacedName) (models []loxiapi.LoadBalancerModel, skipped []error, err error) {
	models = make([]loxiapi.LoadBalancerModel, 0)
	selStr, isSel := ingress.Annotations["loxilb.io/epselect"]
	if !isSel {
		selStr = pkg.EndPointSel_RR
	}

	// Read mTLS configuration from Ingress annotations
	mtlsFrontend, err := r.getMtlsFrontendConfig(ctx, ingress)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get mTLS frontend config: %w", err)
	}

	mtlsBackend, err := r.getMtlsBackendConfig(ctx, ingress)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get mTLS backend config: %w", err)
	}

	timeout := inactiveTimeout(ctx, ingress)

	for _, rule := range ingress.Spec.Rules {
		if rule.HTTP == nil {
			continue
		}

		for _, path := range rule.HTTP.Paths {
			if path.Backend.Service != nil {
				name := path.Backend.Service.Name
				ns := r.getBackendServiceNamespace(ingress, name)
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
				loxisvc.Timeout = timeout
				if owner, taken := owners[rulePathOf(loxisvc)]; taken && owner != client.ObjectKeyFromObject(ingress) {
					skipped = append(skipped, &pathConflictError{path: rulePathOf(loxisvc), owner: owner})
					continue
				}
				loxiep, err := r.createLoxiLoadBalancerEndpointsForBackend(ctx, ns, path.Backend.Service)
				if err != nil {
					skipped = append(skipped, fmt.Errorf("path %s%s: %w", rule.Host, path.Path, err))
					continue
				}

				model := loxiapi.LoadBalancerModel{
					Service:   loxisvc,
					Endpoints: loxiep,
				}
				models = append(models, model)
			}
		}
	}

	return models, skipped, nil
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

	changed := false
	for _, ing := range svc.Status.LoadBalancer.Ingress {
		if r.checkIngressLoadBalancerIngressExist(ingress, ing) {
			continue
		}
		changed = true

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

	if !changed {
		return nil
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

	if err := r.addInitialSyncRunnable(mgr); err != nil {
		return err
	}

	// Only Ingress events are filtered. Secret and EndpointSlice events all go to their mappers,
	// which pick the Ingresses that use the object.
	ingressEvents := predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			ingOld, okOld := e.ObjectOld.(*netv1.Ingress)
			ingNew, okNew := e.ObjectNew.(*netv1.Ingress)
			if !okOld || !okNew {
				return false
			}
			if onlyOthersMarksChanged(ingOld, ingNew, r.ProgrammedKey) {
				return false
			}
			// An Ingress that left the loxilb class still has rules to remove.
			return checkIngClassNameFunc(ingNew) || checkIngClassNameFunc(ingOld)
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			ing, ok := e.Object.(*netv1.Ingress)
			return ok && checkIngClassNameFunc(ing)
		},
		CreateFunc: func(e event.CreateEvent) bool {
			ing, ok := e.Object.(*netv1.Ingress)
			return ok && checkIngClassNameFunc(ing)
		},
		GenericFunc: func(e event.GenericEvent) bool {
			ing, ok := e.Object.(*netv1.Ingress)
			return ok && checkIngClassNameFunc(ing)
		},
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&netv1.Ingress{}, builder.WithPredicates(ingressEvents)).
		// A change of one Ingress can give a rule to, or take one from, another that asks for the
		// same host and path.
		Watches(&netv1.Ingress{}, handler.EnqueueRequestsFromMapFunc(r.ingressesSharingRulePaths), builder.WithPredicates(ingressEvents)).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(secretToIngressMapper)).
		// Backend Pods come and go without the Ingress changing.
		Watches(&discoveryv1.EndpointSlice{}, handler.EnqueueRequestsFromMapFunc(r.ingressesForEndpointSlice)).
		Complete(r)
}

// ingressesForEndpointSlice maps a changed EndpointSlice to the Ingresses that take endpoints
// from its Service.
func (r *LoxilbIngressReconciler) ingressesForEndpointSlice(ctx context.Context, obj client.Object) []reconcile.Request {
	svc := types.NamespacedName{Namespace: obj.GetNamespace(), Name: obj.GetLabels()[discoveryv1.LabelServiceName]}
	if svc.Name == "" {
		return nil
	}

	// Backends may be in another namespace than the Ingress, so every Ingress is looked at.
	ingressList := &netv1.IngressList{}
	if err := r.List(ctx, ingressList); err != nil {
		log.FromContext(ctx).Error(err, "failed to list Ingresses for an EndpointSlice change", "service", svc)
		return nil
	}

	requests := []reconcile.Request{}
	for i := range ingressList.Items {
		ing := &ingressList.Items[i]
		if isLoxilbIngress(ing) && r.usesBackendService(ing, svc) {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(ing)})
		}
	}

	return requests
}

// usesBackendService reports whether the Ingress takes endpoints from the Service.
func (r *LoxilbIngressReconciler) usesBackendService(ingress *netv1.Ingress, svc types.NamespacedName) bool {
	if name, isDirect := ingress.Annotations[pkg.DirectLBServiceAnnotation]; isDirect {
		ns, isNs := ingress.Annotations[pkg.DirectLBNamespaceAnnotation]
		if !isNs {
			ns = ingress.Namespace
		}
		return name == svc.Name && ns == svc.Namespace
	}

	for _, rule := range ingress.Spec.Rules {
		if rule.HTTP == nil {
			continue
		}
		for _, path := range rule.HTTP.Paths {
			backend := path.Backend.Service
			if backend != nil && backend.Name == svc.Name && r.getBackendServiceNamespace(ingress, backend.Name) == svc.Namespace {
				return true
			}
		}
	}

	return false
}
