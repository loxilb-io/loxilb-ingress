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

// Readiness
//
// A loxilb-ingress Pod is ready when its loxilb answers and every loxilb Ingress that existed when
// the controller started has been reconciled once. Until then the Pod has no rules, and a
// connection sent to it would be refused.
//
// An Ingress counts as reconciled after its first attempt whatever the outcome, so one failing
// Ingress can't keep every Pod NotReady. Ingresses created later don't affect readiness.

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	netv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

const loxilbReadyTimeout = 2 * time.Second

// initialSync tracks the first reconcile of the Ingresses that exist at startup. The zero value is
// ready to use.
type initialSync struct {
	mu     sync.Mutex
	listed bool
	// seen holds Ingresses reconciled before the startup list was taken.
	seen    map[types.NamespacedName]struct{}
	pending map[types.NamespacedName]struct{}
}

// expect sets the Ingresses to wait for. Those already reconciled are not waited for.
func (s *initialSync) expect(keys []types.NamespacedName) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.pending = make(map[types.NamespacedName]struct{}, len(keys))
	for _, key := range keys {
		if _, ok := s.seen[key]; !ok {
			s.pending[key] = struct{}{}
		}
	}
	s.seen = nil
	s.listed = true
}

// reconciled records that the Ingress has been reconciled once.
func (s *initialSync) reconciled(key types.NamespacedName) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.listed {
		if s.seen == nil {
			s.seen = make(map[types.NamespacedName]struct{})
		}
		s.seen[key] = struct{}{}
		return
	}
	delete(s.pending, key)
}

// done reports whether the first pass is complete, and otherwise how many Ingresses it waits for.
func (s *initialSync) done() (bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.listed {
		return false, "Ingresses not listed yet"
	}
	if len(s.pending) > 0 {
		return false, fmt.Sprintf("%d Ingresses not reconciled yet", len(s.pending))
	}
	return true, ""
}

// addInitialSyncRunnable lists the loxilb Ingresses once the cache has synced, which starts the
// first reconcile pass that readiness waits for.
func (r *LoxilbIngressReconciler) addInitialSyncRunnable(mgr ctrl.Manager) error {
	return mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		if !mgr.GetCache().WaitForCacheSync(ctx) {
			return fmt.Errorf("cache did not sync")
		}
		return r.listInitialIngresses(ctx)
	}))
}

func (r *LoxilbIngressReconciler) listInitialIngresses(ctx context.Context) error {
	ingresses := &netv1.IngressList{}
	if err := r.List(ctx, ingresses); err != nil {
		return fmt.Errorf("failed to list Ingresses for readiness: %w", err)
	}

	keys := make([]types.NamespacedName, 0, len(ingresses.Items))
	for i := range ingresses.Items {
		if isLoxilbIngress(&ingresses.Items[i]) {
			keys = append(keys, types.NamespacedName{Namespace: ingresses.Items[i].Namespace, Name: ingresses.Items[i].Name})
		}
	}
	r.initialSync.expect(keys)
	return nil
}

// ReadyzCheck is the readiness check of the loxilb-ingress Pod.
func (r *LoxilbIngressReconciler) ReadyzCheck(req *http.Request) error {
	if ok, reason := r.initialSync.done(); !ok {
		return fmt.Errorf("first reconcile pass not finished: %s", reason)
	}

	ctx, cancel := context.WithTimeout(req.Context(), loxilbReadyTimeout)
	defer cancel()
	if _, err := r.LoxiClient.HealthCheck().Get(ctx, ""); err != nil {
		return fmt.Errorf("loxilb is not responding: %w", err)
	}
	return nil
}
