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

// Paths asked for by more than one Ingress
//
// loxilb has one rule per address, port, host, path and path type, and a Pod puts the HTTP rules
// of every Ingress on port 80 and the HTTPS rules on port 443. Two Ingresses with the same host
// and path on a port therefore ask for the same rule. Installing a rule that exists replaces it,
// so each of them took the rule from the other on every reconcile, and deleting the one that had
// it last left the other without a rule.
//
// So the rule goes to one of them: the oldest Ingress that asks for it. The others leave the path
// out, say so in a Warning event, and don't report as programmed. When the owner changes or goes
// away, the others are reconciled again.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	loxiapi "github.com/loxilb-io/kube-loxilb/pkg/api"
	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"loxilb.io/loxilb-ingress-manager/pkg"
)

const pathConflictReason = "PathConflict"

// rulePath is what tells the HTTP and HTTPS rules of a Pod's loxilb apart.
type rulePath struct {
	port      uint16
	host      string
	path      string
	matchMode loxiapi.PathMatchModeType
}

func (p rulePath) String() string {
	return fmt.Sprintf("%s%s (%s) on port %d", p.host, p.path, p.matchMode, p.port)
}

func rulePathOf(service loxiapi.LoadBalancerService) rulePath {
	return rulePath{port: service.Port, host: service.Host, path: service.PathPrefix, matchMode: service.PathMatchMode}
}

// pathConflictError is the reason a path is left out: an older Ingress asks for the same rule.
type pathConflictError struct {
	path  rulePath
	owner types.NamespacedName
}

func (e *pathConflictError) Error() string {
	return fmt.Sprintf("%s is served by the older Ingress %s", e.path, e.owner)
}

// splitPathConflicts separates the paths left out for an older Ingress from the rest.
func splitPathConflicts(skipped []error) (conflicts []*pathConflictError, others []error) {
	for _, err := range skipped {
		if conflict, ok := err.(*pathConflictError); ok {
			conflicts = append(conflicts, conflict)
		} else {
			others = append(others, err)
		}
	}
	return conflicts, others
}

// ingressRulePaths returns the rules the Ingress asks for. It follows createLoxiModelList without
// reading Secrets or endpoints: a path counts whether or not its backend is ready.
func (r *LoxilbIngressReconciler) ingressRulePaths(ingress *netv1.Ingress) []rulePath {
	if _, direct := ingress.Annotations[pkg.DirectLBServiceAnnotation]; direct {
		return nil
	}

	// With mTLS, every rule of the Ingress is an HTTPS rule.
	mtls := ingress.Annotations[mtlsBackendVerifyAnnotation] == "true"
	if mode := ingress.Annotations[mtlsFrontendModeAnnotation]; mode != "" && mode != "disabled" {
		mtls = true
	}

	paths := make([]rulePath, 0)
	for _, rule := range ingress.Spec.Rules {
		if rule.HTTP == nil {
			continue
		}
		port := uint16(80)
		if mtls || r.checkTLSHost(rule.Host, ingress.Spec.TLS) {
			port = 443
		}
		for _, path := range rule.HTTP.Paths {
			if path.Backend.Service == nil {
				continue
			}
			pathType := "prefix"
			if path.PathType != nil {
				pathType = string(*path.PathType)
			}
			paths = append(paths, rulePath{
				port:      port,
				host:      rule.Host,
				path:      path.Path,
				matchMode: loxiapi.PathMatchModeType(strings.ToLower(pathType)),
			})
		}
	}

	return paths
}

// rulePathOwners maps every rule the loxilb Ingresses ask for to the Ingress that gets it: the
// oldest one, in the order used for shared TLS hosts.
func (r *LoxilbIngressReconciler) rulePathOwners(ctx context.Context) (map[rulePath]types.NamespacedName, error) {
	ingresses := &netv1.IngressList{}
	if err := r.List(ctx, ingresses); err != nil {
		return nil, fmt.Errorf("failed to list Ingresses for path owners: %w", err)
	}

	items := ingresses.Items
	sort.Slice(items, func(i, j int) bool { return olderIngress(&items[i], &items[j]) })

	owners := make(map[rulePath]types.NamespacedName)
	for i := range items {
		ing := &items[i]
		if !isLoxilbIngress(ing) {
			continue
		}
		for _, path := range r.ingressRulePaths(ing) {
			if _, taken := owners[path]; !taken {
				owners[path] = client.ObjectKeyFromObject(ing)
			}
		}
	}

	return owners, nil
}

// reportPathConflicts tells the user which paths of the Ingress are left out for an older Ingress.
func (r *LoxilbIngressReconciler) reportPathConflicts(ctx context.Context, ingress *netv1.Ingress, conflicts []*pathConflictError) {
	logger := log.FromContext(ctx)
	for _, conflict := range conflicts {
		logger.Info("leaving out a path that an older Ingress serves", "Ingress", client.ObjectKeyFromObject(ingress),
			"path", conflict.path.String(), "owner", conflict.owner)
		if r.Recorder != nil {
			r.Recorder.Event(ingress, corev1.EventTypeWarning, pathConflictReason, conflict.Error())
		}
	}
}

// ingressesSharingRulePaths maps a changed Ingress to the other Ingresses that ask for one of its
// rules, because the change may give them a rule or take one from them.
func (r *LoxilbIngressReconciler) ingressesSharingRulePaths(ctx context.Context, obj client.Object) []reconcile.Request {
	changed, ok := obj.(*netv1.Ingress)
	if !ok {
		return nil
	}

	// The paths count also when the Ingress has left the loxilb class: the others can have them now.
	wanted := make(map[rulePath]struct{})
	for _, path := range r.ingressRulePaths(changed) {
		wanted[path] = struct{}{}
	}
	if len(wanted) == 0 {
		return nil
	}

	ingressList := &netv1.IngressList{}
	if err := r.List(ctx, ingressList); err != nil {
		log.FromContext(ctx).Error(err, "failed to list Ingresses for an Ingress change", "Ingress", client.ObjectKeyFromObject(changed))
		return nil
	}

	requests := []reconcile.Request{}
	for i := range ingressList.Items {
		ing := &ingressList.Items[i]
		if !isLoxilbIngress(ing) || client.ObjectKeyFromObject(ing) == client.ObjectKeyFromObject(changed) {
			continue
		}
		for _, path := range r.ingressRulePaths(ing) {
			if _, shared := wanted[path]; shared {
				requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(ing)})
				break
			}
		}
	}

	return requests
}
