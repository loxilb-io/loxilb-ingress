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

// Programmed report
//
// Every loxilb-ingress Pod installs the rules of every Ingress into its own loxilb, so whether an
// Ingress is being served depends on the Pod. Each Pod records what it has installed on the Ingress:
//
//	metadata.annotations["loxilb.io/programmed.<Pod name>"] = "<metadata.generation>"
//
// The value is the generation whose spec the Pod has installed. It is written only after the rules
// were installed, and removed when the Pod fails to install them or drops them. Each Pod writes
// only its own key with a JSON merge patch, so Pods never overwrite each other. Pods that report
// carry the label loxilb.io/programmed-report=v1 (set in the manifest).

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	netv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const programmedAnnotationPrefix = "loxilb.io/programmed."

// ProgrammedAnnotationKey returns the annotation key a loxilb-ingress Pod uses to report the
// Ingress generations it has installed.
func ProgrammedAnnotationKey(podName string) (string, error) {
	key := programmedAnnotationPrefix + podName
	if errs := validation.IsQualifiedName(key); len(errs) > 0 {
		return "", fmt.Errorf("pod name %q does not fit in annotation key %q: %s", podName, key, strings.Join(errs, "; "))
	}
	return key, nil
}

func isProgrammedAnnotation(key string) bool {
	return strings.HasPrefix(key, programmedAnnotationPrefix)
}

// markProgrammed records that this Pod has installed the rules for the Ingress's current generation.
func (r *LoxilbIngressReconciler) markProgrammed(ctx context.Context, ingress *netv1.Ingress) error {
	if r.ProgrammedKey == "" {
		return nil
	}
	value := strconv.FormatInt(ingress.Generation, 10)
	if current, ok := ingress.Annotations[r.ProgrammedKey]; ok && current == value {
		return nil
	}
	return patchProgrammedAnnotation(ctx, r.Client, ingress, r.ProgrammedKey, &value)
}

// clearProgrammed removes this Pod's mark when its rules for the Ingress are not installed.
// It patches even if the cached Ingress has no mark: the cache may not have seen a mark this Pod
// wrote moments ago. Removing a missing key is a no-op on the API server.
func (r *LoxilbIngressReconciler) clearProgrammed(ctx context.Context, ingress *netv1.Ingress) error {
	if r.ProgrammedKey == "" {
		return nil
	}
	return patchProgrammedAnnotation(ctx, r.Client, ingress, r.ProgrammedKey, nil)
}

// patchProgrammedAnnotation sets key to *value, or removes it when value is nil.
func patchProgrammedAnnotation(ctx context.Context, c client.Writer, ingress *netv1.Ingress, key string, value *string) error {
	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{
			"annotations": map[string]*string{key: value},
		},
	})
	if err != nil {
		return err
	}
	if err := c.Patch(ctx, ingress, client.RawPatch(types.MergePatchType, patch)); err != nil && !errors.IsNotFound(err) {
		return err
	}
	return nil
}

// ClearProgrammedAnnotations removes the marks under key from every Ingress. It runs before the
// controller starts: a restarted container keeps its Pod name but starts with an empty loxilb, so
// marks left by the previous run would claim rules that are no longer installed.
func ClearProgrammedAnnotations(ctx context.Context, reader client.Reader, writer client.Writer, key string) error {
	ingresses := &netv1.IngressList{}
	if err := reader.List(ctx, ingresses); err != nil {
		return err
	}

	for i := range ingresses.Items {
		ingress := &ingresses.Items[i]
		if _, ok := ingress.Annotations[key]; !ok {
			continue
		}
		if err := patchProgrammedAnnotation(ctx, writer, ingress, key, nil); err != nil {
			return fmt.Errorf("failed to clear %s on ingress %s/%s: %w", key, ingress.Namespace, ingress.Name, err)
		}
	}
	return nil
}

// onlyOthersMarksChanged reports whether an Ingress update changed nothing but programmed
// annotations, while this Pod's own mark stayed as it was or now matches the generation (that is
// the update this Pod's own patch makes). Such updates need no reconcile; without this filter
// every Pod's mark would make every Pod reconcile the Ingress again. If someone else removes or
// changes this Pod's mark, the update still goes through so the mark is restored.
func onlyOthersMarksChanged(oldIng, newIng *netv1.Ingress, ownKey string) bool {
	oldMarks, oldRest := splitProgrammedAnnotations(oldIng.Annotations)
	newMarks, newRest := splitProgrammedAnnotations(newIng.Annotations)
	if equality.Semantic.DeepEqual(oldMarks, newMarks) {
		return false
	}

	if ownKey != "" && oldMarks[ownKey] != newMarks[ownKey] &&
		newMarks[ownKey] != strconv.FormatInt(newIng.Generation, 10) {
		return false
	}

	oldCopy := oldIng.DeepCopy()
	newCopy := newIng.DeepCopy()
	oldCopy.Annotations, newCopy.Annotations = oldRest, newRest
	oldCopy.ResourceVersion, newCopy.ResourceVersion = "", ""
	oldCopy.ManagedFields, newCopy.ManagedFields = nil, nil
	return equality.Semantic.DeepEqual(oldCopy, newCopy)
}

func splitProgrammedAnnotations(annotations map[string]string) (marks, rest map[string]string) {
	marks = map[string]string{}
	rest = map[string]string{}
	for k, v := range annotations {
		if isProgrammedAnnotation(k) {
			marks[k] = v
		} else {
			rest[k] = v
		}
	}
	return marks, rest
}
