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
	"strings"
	"time"

	loxiapi "github.com/loxilb-io/kube-loxilb/pkg/api"
	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"loxilb.io/loxilb-ingress-manager/pkg"
)

const (
	loxilbIngressClassName = "loxilb"
)

type LoxilbIngressReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	LoxiClient *loxiapi.LoxiClient
}

func (r *LoxilbIngressReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	ruleName := fmt.Sprintf("%s_%s", req.Namespace, req.Name)
	ruleNameHTTPS := fmt.Sprintf("%s_%s_https", req.Namespace, req.Name)

	currLBList, err := r.LoxiClient.LoadBalancer().List(ctx)
	if err != nil {
		logger.Info("Failed to get existing loxilb-ingress rules")
		return ctrl.Result{}, err
	}

	exist := false
	existHTTPS := false
	for _, lbItem := range currLBList.Item {
		if lbItem.Service.Name == ruleName {
			exist = true
		} else if lbItem.Service.Name == ruleNameHTTPS {
			existHTTPS = true
		}
	}

	ingress := &netv1.Ingress{}
	err = r.Get(ctx, req.NamespacedName, ingress)
	if err != nil {
		// Ingress is deleted.
		if errors.IsNotFound(err) {
			logger.Info("This resource is deleted", "Ingress", req.NamespacedName)
			if exist {
				if err := r.LoxiClient.LoadBalancer().DeleteByName(ctx, ruleName); err != nil {
					logger.Error(err, "failed to delete loxilb-ingress rule "+ruleName)
				}
			}
			if existHTTPS {
				if err := r.LoxiClient.LoadBalancer().DeleteByName(ctx, ruleNameHTTPS); err != nil {
					logger.Error(err, "failed to delete loxilb-ingress rule "+ruleNameHTTPS)
				}
			}
			return ctrl.Result{}, nil
		}

		logger.Error(err, "Failed to get ingress", "ingress", ingress)
		return ctrl.Result{}, err
	}

	// when ingress is added, install rule to loxilb-ingress
	var models []loxiapi.LoadBalancerModel
	if _, isok := ingress.Annotations["loxilb.io/direct-loadbalance-service"]; isok {
		models, err = r.createDirectLoxiModelList(ctx, ingress)
	} else {
		models, err = r.createLoxiModelList(ctx, ingress)
	}

	if err != nil {
		if exist {
			if err := r.LoxiClient.LoadBalancer().DeleteByName(ctx, ruleName); err == nil {
				logger.Info("deleted loxilb-ingress rule ", ruleName, "no endpoints")
			}
		}
		if existHTTPS {
			if err := r.LoxiClient.LoadBalancer().DeleteByName(ctx, ruleNameHTTPS); err == nil {
				logger.Info("deleted loxilb-ingress rule ", ruleNameHTTPS, "no endpoints")
			}
		}
		logger.Error(err, "Failed to set ingress. failed to create loxilb loadbalancer model", "[]loxiapi.LoadBalancerModel", models)
		return ctrl.Result{}, err
	}

	var applyModels []loxiapi.LoadBalancerModel
nextModel:
	for _, model := range models {
		for _, lbItem := range currLBList.Item {
			if lbItem.Service.Name == model.Service.Name && len(lbItem.Endpoints) == len(model.Endpoints) {
				match := true
				for _, mep := range model.Endpoints {
					epMatch := false
					for _, ep := range lbItem.Endpoints {
						if mep.EndpointIP == ep.EndpointIP && mep.TargetPort == ep.TargetPort {
							epMatch = true
							break
						}
					}
					if !epMatch {
						match = false
						break
					}
				}
				if match {
					continue nextModel
				}
			}
		}
		applyModels = append(applyModels, model)
	}

	if len(applyModels) <= 0 {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	logger.Info("createLoxiModelList return models:", "[]loxiapi.LoadBalancerModel", applyModels)

	for _, model := range applyModels {
		err = r.LoxiClient.LoadBalancer().Create(ctx, &model)
		if err != nil {
			if err.Error() != "lbrule-exists error" {
				logger.Error(err, "failed to install loadbalancer rule to loxilb", "loxiapi.LoadBalancerModel", model)
				return ctrl.Result{}, err
			}
		}
	}

	if err := r.updateIngressStatus(ctx, ingress); err != nil {
		logger.Info("failed to update ingress status.", "error", err)
	}

	logger.Info("This resource is created", "ingress", ingress)
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
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

func (r *LoxilbIngressReconciler) createLoxiLoadBalancerService(ns, name, externalIP string, security int32, host string) loxiapi.LoadBalancerService {
	service := loxiapi.LoadBalancerService{
		ExternalIP: externalIP,
		Protocol:   "tcp",
		Mode:       4, // fullproxy mode
		Name:       fmt.Sprintf("%s_%s", ns, name),
		Host:       host,
		Security:   security,
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
	key := types.NamespacedName{
		Namespace: ns,
		Name:      name,
	}

	ep := &corev1.Endpoints{}
	if err := r.Get(ctx, key, ep); err != nil {
		return loxilbEpList, err
	}

	for _, subset := range ep.Subsets {
		for _, addr := range subset.Addresses {
			for _, port := range subset.Ports {
				loxilbEp := loxiapi.LoadBalancerEndpoint{
					EndpointIP: addr.IP,
					TargetPort: uint16(port.Port),
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

func (r *LoxilbIngressReconciler) createLoxiLoadBalancerEndpointsWithTargetPort(ctx context.Context, ns, name string, targetPort int32) ([]loxiapi.LoadBalancerEndpoint, error) {
	loxilbEpList := make([]loxiapi.LoadBalancerEndpoint, 0)
	key := types.NamespacedName{
		Namespace: ns,
		Name:      name,
	}

	ep := &corev1.Endpoints{}
	if err := r.Get(ctx, key, ep); err != nil {
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
		loxisvc := r.createDirectLoxiLoadBalancerService(svcNs, lbName, "0.0.0.0", protocol, "", selStr, port.Port)
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
				if r.checkTLSHost(rule.Host, ingress.Spec.TLS) {
					security = 1
				}

				lbName := ingress.Name
				if security == 1 {
					lbName += "_https"
				}
				loxisvc := r.createLoxiLoadBalancerService(ingress.Namespace, lbName, r.LoxiClient.Host, security, rule.Host)
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
	checkIngClassNameFunc := func(ing *netv1.Ingress) bool {
		if ing.Spec.IngressClassName != nil {
			if *ing.Spec.IngressClassName == loxilbIngressClassName {
				return true
			}
		}
		return false
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&netv1.Ingress{}).
		WithEventFilter(predicate.Funcs{
			UpdateFunc: func(e event.UpdateEvent) bool {
				return false
			},
			DeleteFunc: func(e event.DeleteEvent) bool {
				ing, ok := e.Object.(*netv1.Ingress)
				if ok {
					return checkIngClassNameFunc(ing)
				}
				return false
			},
			CreateFunc: func(e event.CreateEvent) bool {
				ing, ok := e.Object.(*netv1.Ingress)
				if ok {
					return checkIngClassNameFunc(ing)
				}
				return false
			},
			GenericFunc: func(e event.GenericEvent) bool {
				ing, ok := e.Object.(*netv1.Ingress)
				if ok {
					return checkIngClassNameFunc(ing)
				}
				return false
			},
		}).
		Complete(r)
}
