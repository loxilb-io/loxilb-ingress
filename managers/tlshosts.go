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

// TLS hosts shared by Ingresses
//
// loxilb keeps one certificate per host, and several Ingresses may list the same host under
// spec.tls: kube-loxilb makes one Ingress per HTTPRoute, and the routes of a Gateway listener
// share its hostname. So:
//
//   - The certificate of a host comes from one Ingress, the oldest one that lists the host with a
//     Secret. The others register theirs only while the host has none, so Ingresses with
//     different Secrets for a host don't keep replacing each other's certificate.
//   - The certificate is removed when the last Ingress that lists the host is gone, not when any
//     of them is.

import (
	"context"
	"fmt"
	"sort"

	netv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// tlsHostProviders maps every TLS host of the loxilb Ingresses to the Ingress that provides its
// certificate. A host that no Ingress lists with a Secret is not in the map.
func (r *LoxilbIngressReconciler) tlsHostProviders(ctx context.Context) (map[string]types.NamespacedName, error) {
	ingresses := &netv1.IngressList{}
	if err := r.List(ctx, ingresses); err != nil {
		return nil, fmt.Errorf("failed to list Ingresses for TLS hosts: %w", err)
	}

	items := ingresses.Items
	sort.Slice(items, func(i, j int) bool { return olderIngress(&items[i], &items[j]) })

	providers := make(map[string]types.NamespacedName)
	for i := range items {
		ing := &items[i]
		if !isLoxilbIngress(ing) {
			continue
		}
		for _, tls := range ing.Spec.TLS {
			if tls.SecretName == "" {
				continue
			}
			for _, host := range tls.Hosts {
				if _, taken := providers[host]; !taken {
					providers[host] = client.ObjectKeyFromObject(ing)
				}
			}
		}
	}

	return providers, nil
}

// olderIngress orders Ingresses by creation time, and by namespace and name when that is equal.
func olderIngress(a, b *netv1.Ingress) bool {
	if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return a.CreationTimestamp.Before(&b.CreationTimestamp)
	}
	if a.Namespace != b.Namespace {
		return a.Namespace < b.Namespace
	}
	return a.Name < b.Name
}
