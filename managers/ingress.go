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

	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	loxiapi "github.com/loxilb-io/kube-loxilb/pkg/api"
)

type LoxilbIngressReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	LoxiClient *loxiapi.LoxiClient
}

func (r *LoxilbIngressReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	ingress := &netv1.Ingress{}
	err := r.Client.Get(ctx, req.NamespacedName, ingress)
	if err != nil {
		// Ingress is deleted.
		if errors.IsNotFound(err) {
			logger.Info("This resource is deleted", "Ingress", req.NamespacedName)
			// TODO: when deleted
			r.LoxiClient.LoadBalancer().DeleteByName(ctx, fmt.Sprintf("%s_%s", req.Namespace, req.Name))
			return ctrl.Result{}, nil
		}

		logger.Error(err, "Failed to get ingress %s/%s", req.Namespace, req.Name)
		return ctrl.Result{}, err
	}

	// when ingress is added, install rule to loxilb-ingress
	models, err := r.createLoxiModelList(ctx, ingress)
	if err != nil {
		logger.Error(err, "Failed to set ingress %s/%s. failed to create loxilb loadbalancer model", req.Namespace, req.Name)
	}

	for _, model := range models {
		err = r.LoxiClient.LoadBalancer().Create(ctx, &model)
		if err != nil {
			logger.Error(err, "Failed to set ingress %s/%s. failed to install loadbalancer rule to loxilb", req.Namespace, req.Name)
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

func (r *LoxilbIngressReconciler) createLoxiLoadBalancerService(ns, name, externalIP string, port int32, host string) loxiapi.LoadBalancerService {
	service := loxiapi.LoadBalancerService{
		ExternalIP: externalIP,
		Protocol:   "tcp",
		Port:       uint16(port),
		Mode:       4, // fullproxy mode
		Name:       fmt.Sprintf("%s_%s", ns, name),
		Host:       host,
	}

	return service
}

func (r *LoxilbIngressReconciler) createLoxiLoadBalancerEndpoints(ctx context.Context, ns, name string, port int32) ([]loxiapi.LoadBalancerEndpoint, error) {
	loxilbEpList := make([]loxiapi.LoadBalancerEndpoint, 0)
	key := types.NamespacedName{
		Namespace: ns,
		Name:      name,
	}

	svc := &corev1.Service{}
	if err := r.Client.Get(ctx, key, svc); err != nil {
		return loxilbEpList, err
	}

	targetPort := 0
	for _, svcPort := range svc.Spec.Ports {
		if svcPort.Port == port {
			targetPortNum, err := r.getServicePortIntValue(ctx, svc, svcPort)
			if err != nil {
				return loxilbEpList, err
			}
			targetPort = targetPortNum
			break
		}
	}

	ep := &corev1.Endpoints{}
	if err := r.Client.Get(ctx, key, ep); err != nil {
		return loxilbEpList, err
	}

	for _, subset := range ep.Subsets {
		for _, addr := range subset.Addresses {
			loxilbEp := loxiapi.LoadBalancerEndpoint{
				EndpointIP: addr.IP,
				TargetPort: uint16(targetPort),
				Weight:     uint8(1),
			}
			loxilbEpList = append(loxilbEpList, loxilbEp)
		}
	}

	return loxilbEpList, nil
}

func (r *LoxilbIngressReconciler) getServicePortIntValue(ctx context.Context, svc *corev1.Service, port corev1.ServicePort) (int, error) {
	if port.TargetPort.IntValue() != 0 {
		return port.TargetPort.IntValue(), nil
	}

	selectorLabel := labels.SelectorFromSet(svc.Spec.Selector)
	pods := &corev1.PodList{}
	if err := r.Client.List(ctx, pods, client.InNamespace(svc.Namespace), &client.ListOptions{LabelSelector: selectorLabel}); err != nil {
		return 0, err
	}

	for _, pod := range pods.Items {
		for _, c := range pod.Spec.Containers {
			for _, p := range c.Ports {
				if p.Name == port.TargetPort.String() {
					return int(p.ContainerPort), nil
				}
			}
		}
	}

	return 0, fmt.Errorf("not found port name %s in service %s", port.TargetPort.String(), svc.Name)
}

func (r *LoxilbIngressReconciler) checkTlsHost(host string, TLS []netv1.IngressTLS) bool {
	for _, tls := range TLS {
		for _, tlsHost := range tls.Hosts {
			if host == tlsHost {
				return true
			}
		}
	}
	return false
}

func (r *LoxilbIngressReconciler) getBackendServiceNamespace(ingress *netv1.Ingress, backendName string) string {
	if _, isok := ingress.Annotations["external-backend-service"]; isok {
		if backendNamespace, isNs := ingress.Annotations["service-"+backendName+"-namespace"]; isNs {
			return backendNamespace
		}
	}
	return ingress.Namespace
}

func (r *LoxilbIngressReconciler) createLoxiModelList(ctx context.Context, ingress *netv1.Ingress) ([]loxiapi.LoadBalancerModel, error) {
	models := make([]loxiapi.LoadBalancerModel, 0)
	for _, rule := range ingress.Spec.Rules {
		for _, path := range rule.HTTP.Paths {
			if path.Backend.Service != nil {
				name := path.Backend.Service.Name
				ns := r.getBackendServiceNamespace(ingress, name)
				port := path.Backend.Service.Port.Number

				loxisvc := r.createLoxiLoadBalancerService(ns, name, r.LoxiClient.Host, port, rule.Host)
				if r.checkTlsHost(rule.Host, ingress.Spec.TLS) {
					loxisvc.Security = 1
				}

				loxiep, err := r.createLoxiLoadBalancerEndpoints(ctx, ns, name, port)
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

func (r *LoxilbIngressReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&netv1.Ingress{}).
		Complete(r)
}
