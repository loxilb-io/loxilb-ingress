[![Docker-Multi-Arch](https://github.com/loxilb-io/loxilb-ingress/actions/workflows/docker-multiarch.yml/badge.svg)](https://github.com/loxilb-io/loxilb-ingress/actions/workflows/docker-multiarch.yml) [![K3s-Loxi-Ingress-Sanity-CI](https://github.com/loxilb-io/loxilb/actions/workflows/k3s-loxi-ingress.yml/badge.svg)](https://github.com/loxilb-io/loxilb/actions/workflows/k3s-loxi-ingress.yml)   

## How to run loxilb-ingress

In Kubernetes, there is usually a lot of overlap between network load-balancer and an Ingress functionality. This creates a lot of confusion. Overall, the differences between an Ingress and a load-balancer service can be categorized as follows:

| Feature       | Ingress       | Load-balancer |
| ------------- | ------------- |-------------- |
| Protocol      | HTTP level (Layer7)  | Network Layer4   |
| Additional Features | Ingress Rules, Resource-Backends, | Based on L4 Session Params |
||Path-Types, HostName   |   |
| Yaml Manifest| apiVersion: networking.k8s.io/v2 | type: LoadBalancer|

When using Ingress, the clients connect to one of the pods through Ingress. The clients first perform a DNS lookup which returns IP address of the ingress. This IP address is usually funnelled through a L4 Load-balancer. The client sends a HTTP(s) request to Ingress specifying URL, hostname and other HTTP headers. Based on the HTTP payload, the ingress finds an associated Service and its EndPoint Objects. The Ingress then forwards the client's request to appopriate pod. It can also serve as HTTS termination point or as a mTLS hub.

<img src="https://github.com/user-attachments/assets/b8d0f0a0-c298-492e-8647-7c5936a87ea9" width="500" >


With Kubernetes ingress, we can expose multiple paths with the same service IP. This might be helpful if one is using public cloud, where one has to pay for managed LB services. Hence, creating a single service and exposing mulitple URL paths might be optimal in such use-cases.

loxilb-ingress is optimized for cases which require long-lived connections and https termination with eBPF.

### Getting Started

The following getting started example is  based on  [K3s](https://k3s.io/) as the kubernetes platform but should work on any kubernetes implementation or distribution like EKS, GKE etc but should work well with any. We will use K3s as the kubernetes platform.

### Install K3s
```
curl -sfL https://get.k3s.io | INSTALL_K3S_EXEC="--disable traefik,servicelb" K3S_KUBECONFIG_MODE="644" sh -
```

### Install loxilb as a L4 service LB

Follow any of the getting started [guides](https://github.com/loxilb-io/loxilb?tab=readme-ov-file#getting-started) as per requirement. In this example, we will run loxilb-lb in external mode. Check all the pods are up and running as expected :

```
$ kubectl get pods -A
NAMESPACE     NAME                                     READY   STATUS    RESTARTS   AGE
kube-system   coredns-6799fbcd5-cp5lv                  1/1     Running   0          3h26m
kube-system   kube-loxilb-755f6fb85-gbg7f              1/1     Running   0          3h26m
kube-system   local-path-provisioner-6f5d79df6-47n2b   1/1     Running   0          3h26m
kube-system   metrics-server-54fd9b65b-b6c6x           1/1     Running   0          3h26m
```

### Prepare TLS/SSL certificates for Ingress

Self-signed TLS/SSL certificates and private keys can be built using various tools like [OpenSSL](https://www.openssl.org) or [Minica](https://github.com/jsha/minica). Basically, one will need to have two files -  server.crt and server.key  for loxilb-ingress usage. Once these files are in place, a Kubernetes secret can be created using the following yaml:
```
apiVersion: v1
data:
  server.crt: LS0tLS1CRUdJTiBDRVJUSUZJQ0FURS0tLS0tCk1JSUI3RENDQVhPZ0F3SUJBZ0lJU.....
  server.key: LS0tLS1CRUdJTiBQUklWQVRFIEtFWS0tLS0tCk1JRzJBZ0VBTUJBR0J5cUdTTTQ5Q.....
kind: Secret
metadata:
  creationTimestamp: null
  name: loxilb-ssl
  namespace: kube-system
type: Opaque
```

Please note the above are just dummy values but they need to be in base64 format. How do you get the base64 values from server.crt and server.key files ?

```
$ base64 server.crt
LS0tLS1CRUdJTiBDRVJUSUZJQ0FURS0tLS0tCk1JSUI3RENDQVhPZ0F3SUJBZ0lJU.....
$ base64 server.key
LS0tLS1CRUdJTiBQUklWQVRFIEtFWS0tLS0tCk1JRzJBZ0VBTUJBR0J5cUdTTTQ5Q.....
```

After applying the yaml, we can check the created secret :
```
$ kubectl get secret -n kube-system loxilb-ssl
NAME         TYPE     DATA   AGE
loxilb-ssl   Opaque   2      106m
```

In the subsequent steps, this secret ```loxilb-ssl``` will be used throughout.


### Install loxilb-ingress

```
kubectl apply -f https://raw.githubusercontent.com/loxilb-io/loxilb-ingress/main/manifests/loxilb-ingress-deploy.yml
```

Check status of running pods :
```
$ kubectl get pods -A
NAMESPACE       NAME                                        READY   STATUS      RESTARTS   AGE
kube-system   coredns-6799fbcd5-cp5lv                  1/1     Running   0          3h26m
kube-system   kube-loxilb-755f6fb85-gbg7f              1/1     Running   0          3h26m
kube-system   local-path-provisioner-6f5d79df6-47n2b   1/1     Running   0          3h26m
kube-system   loxilb-ingress-hn5ld                     1/1     Running   0          61m
kube-system   metrics-server-54fd9b65b-b6c6x           1/1     Running   0          3h26m
```

### Install service, backend app and ingress rules

* Create a LB service for exposing ingress ports

```
apiVersion: v1
kind: Service
metadata:
  name: loxilb-ingress-manager
  namespace: kube-system
  annotations:
    loxilb.io/lbmode: "onearm"
spec:
  externalTrafficPolicy: Local
  loadBalancerClass: loxilb.io/loxilb
  selector:
    app.kubernetes.io/instance: loxilb-ingress
    app.kubernetes.io/name: loxilb-ingress
  ports:
    - name: http
      port: 80
      protocol: TCP
      targetPort: 80
    - name: https
      port: 443
      protocol: TCP
      targetPort: 443
  type: LoadBalancer
```

Check the services created :

```
$ kubectl get svc -A
NAMESPACE     NAME                     TYPE           CLUSTER-IP     EXTERNAL-IP        PORT(S)                      AGE
default       kubernetes               ClusterIP      10.43.0.1      <none>             443/TCP                      3h28m
kube-system   kube-dns                 ClusterIP      10.43.0.10     <none>             53/UDP,53/TCP,9153/TCP       3h28m
kube-system   loxilb-ingress-manager   LoadBalancer   10.43.136.1    llb-192.168.80.9   80:31686/TCP,443:31994/TCP   62m
kube-system   metrics-server           ClusterIP      10.43.236.60   <none>             443/TCP                      3h28m
```

At this point of time, all services exposed via ingress can be accessed via "192.168.80.9". This IP could be different as per use-case and scenario. This IP can then be associated with DNS for name based access.


* Create backend apps for ```domain1.loxilb.io``` and configure ingress rules with the following yaml :   
```
apiVersion: apps/v1
kind: Deployment
metadata:
  name: site
spec:
  replicas: 1
  selector:
    matchLabels:
      name: site-handler
  template:
    metadata:
      labels:
        name: site-handler
    spec:
      containers:
        - name: blog
          image: ghcr.io/loxilb-io/nginx:stable
          imagePullPolicy: Always
          ports:
            - containerPort: 80
---
apiVersion: v1
kind: Service
metadata:
  name: site-handler-service
spec:
  ports:
    - name: http
      port: 80
      protocol: TCP
      targetPort: 80
  selector:
    name: site-handler
---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: site-loxilb-ingress
spec:
  ingressClassName: loxilb
  tls:
  - hosts:
    - domain1.loxilb.io
    secretName: loxilb-ssl
  rules:
  - host: domain1.loxilb.io
    http:
      paths:
        - path: /
          pathType: Prefix
          backend:
            service:
              name: site-handler-service
              port:
                number: 80
```

Double check status of pods, services and ingress:

```
$ kubectl get pods -A
NAMESPACE     NAME                                     READY   STATUS    RESTARTS   AGE
default       site-869fd54548-t82bq                    1/1     Running   0          64m
kube-system   coredns-6799fbcd5-cp5lv                  1/1     Running   0          3h31m
kube-system   kube-loxilb-755f6fb85-gbg7f              1/1     Running   0          3h31m
kube-system   local-path-provisioner-6f5d79df6-47n2b   1/1     Running   0          3h31m
kube-system   loxilb-ingress-hn5ld                     1/1     Running   0          66m
kube-system   metrics-server-54fd9b65b-b6c6x           1/1     Running   0          3h31m

$ kubectl get svc -A
NAMESPACE     NAME                     TYPE           CLUSTER-IP     EXTERNAL-IP        PORT(S)                      AGE
default       kubernetes               ClusterIP      10.43.0.1      <none>             443/TCP                      3h31m
default       site-handler-service     ClusterIP      10.43.101.77   <none>             80/TCP                       64m
kube-system   kube-dns                 ClusterIP      10.43.0.10     <none>             53/UDP,53/TCP,9153/TCP       3h31m
kube-system   loxilb-ingress-manager   LoadBalancer   10.43.136.1    llb-192.168.80.9   80:31686/TCP,443:31994/TCP   65m
kube-system   metrics-server           ClusterIP      10.43.236.60   <none>             443/TCP                      3h31m


$ kubectl get ingress -A
NAMESPACE   NAME                  CLASS    HOSTS               ADDRESS   PORTS     AGE
default     site-loxilb-ingress   <none>   domain1.loxilb.io             80, 443   65m
```


We can for the above example and create backend apps for other hostnames e.g. ```domain2.loxilb.io``` and configure ingress rules.

## Testing the ingress 

If you are testing locally you can simply add the following for dns resolution in your bastion/host :

```
$ tail -n 2  /etc/hosts
192.168.80.9   domain1.loxilb.io

```
The above step is similar to adding A records in a DNS like route53.

* Finally, try to access the service "domain1.loxilb.io" :
```
$ curl -H "HOST: domain1.loxilb.io" https://domain1.loxilb.io
<!DOCTYPE html>
<html>
<head>
<title>Welcome to nginx!</title>
<style>
html { color-scheme: light dark; }
body { width: 35em; margin: 0 auto;
font-family: Tahoma, Verdana, Arial, sans-serif; }
</style>
</head>
<body>
<h1>Welcome to nginx!</h1>
<p>If you see this page, the nginx web server is successfully installed and
working. Further configuration is required.</p>

<p>For online documentation and support please refer to
<a href="http://nginx.org/">nginx.org</a>.<br/>
Commercial support is available at
<a href="http://nginx.com/">nginx.com</a>.</p>

<p><em>Thank you for using nginx.</em></p>
</body>
</html>
```

## Checking which Pods have installed an Ingress

Each loxilb-ingress Pod installs every Ingress into its own loxilb. After a Pod has installed an Ingress, it writes the Ingress's `metadata.generation` to an annotation named after the Pod:

```
$ kubectl get ingress site -o jsonpath='{.metadata.generation}{"\n"}{.metadata.annotations}{"\n"}'
2
{"loxilb.io/programmed.loxilb-ingress-x7k2p":"2","loxilb.io/programmed.loxilb-ingress-m4q9z":"1"}
```

Here `loxilb-ingress-x7k2p` serves the current spec, and `loxilb-ingress-m4q9z` hasn't installed it yet.

* The value is written only after the rules are installed. If some paths' backends can't be resolved yet (no Service, no such port, or no endpoints), the other paths are installed and the value is written; those paths can't be served until their backends are ready. A Pod removes its annotation when it can't install any path of the Ingress, or when it removes the rules.
* When a container restarts, it removes its annotations at startup and writes them again after it reinstalls the rules. Annotations of Pods that no longer exist are left in place.
* Pods that report carry the label `loxilb.io/programmed-report: "v1"`. Reporting needs the `POD_NAME` env and `patch` on `ingresses`, both in [manifests/loxilb-ingress-deploy.yml](manifests/loxilb-ingress-deploy.yml).
* The Pod name must be 52 characters or shorter, because the part of the key after `loxilb.io/` can be at most 63 characters. A Pod with a longer name logs an error and doesn't report.

## Backends that aren't ready and Pod readiness

* A path whose backend can't be resolved yet is left out, and the other paths of the Ingress stay installed. The Ingress is reconciled again every 5 seconds until all of its paths are installed.
* A loxilb-ingress Pod becomes Ready once its loxilb answers and every loxilb Ingress that existed when the controller started has been reconciled once. An Ingress that fails counts once it has been tried, so it doesn't keep Pods NotReady. The check is served at `:8081/readyz` and used by the `readinessProbe` in [manifests/loxilb-ingress-deploy.yml](manifests/loxilb-ingress-deploy.yml).
* When the Pods behind a backend Service change, the Ingresses that use the Service are reconciled right away. Every Ingress is also reconciled every 30 seconds.

## Inactivity timeout

loxilb closes a connection that has carried no data for the rule's inactivity timeout. The HTTP and HTTPS rules of an Ingress get 1800 seconds (30 minutes), the same default kube-loxilb gives its own rules. Set `loxilb.io/timeout` on the Ingress to change it:

```
metadata:
  annotations:
    loxilb.io/timeout: "7200"   # seconds
```

* Connections that stay open and can be quiet for long, such as a WebSocket behind a web terminal, need a timeout longer than their quiet periods.
* The largest value is 86400 (24 hours); a larger one is lowered to it. A value that isn't a positive number of seconds is ignored, and the default is used.
* kube-loxilb copies the annotations of an HTTPRoute to the Ingress it makes for it, so the annotation can be set on the HTTPRoute.
* Rules of a `loxilb.io/direct-loadbalance-service` Ingress are not affected. They keep loxilb's own default.

**Limitation: the value is per port, not per Ingress.** loxilb (v0.9.9.1, the current base image) doesn't take the timeout of a connection from the rule that serves it. When it accepts a connection, it takes the timeout of the oldest rule on the port. All HTTP rules of a Pod are on port 80 and all HTTPS rules are on port 443, so:

* The annotation of an Ingress applies to every connection on the port while that Ingress has the oldest rule there. Otherwise it applies to none, also not to the Ingress's own connections.
* Changing the annotation installs the rules of the Ingress again, which makes them the newest on the port. The new value then usually doesn't apply.
* An Ingress with a short timeout and the oldest rule closes the quiet connections of every Ingress on the port.
* The timeout is dependable only when every Ingress on the port has the same value. That is the case when none has the annotation: every connection gets 1800 seconds.

## TLS certificates

loxilb keeps one certificate per host. loxilb-ingress writes the certificate of each `spec.tls` host to `/opt/loxilb/cert/<host>/` and registers it with loxilb.

* **Hosts shared by Ingresses.** Several Ingresses may list the same host under `spec.tls`. The certificate then comes from the oldest of them that lists the host with a `secretName` (by creation time, then namespace and name); the certificate of another is used only while the host has none. It is removed when the last of them is deleted, not when any of them is.
* **Renewal.** When the Secret's certificate or key changes, loxilb-ingress registers the new certificate. loxilb can't replace a registered certificate, so the old one is removed and the new one is registered right after it. Between the two calls, a new connection for the host gets the certificate of the port's listener instead of its own. That is the certificate of the rule the listener was created for, so usually another host's, and a client that verifies the certificate fails the handshake.
* If the new certificate and key don't match, the certificate in use is kept, and the Ingress reports an error until the Secret is fixed.

## Wildcard hosts

loxilb-ingress passes the host of an Ingress rule and of `spec.tls` to loxilb as it is written. What a wildcard host such as `*.example.com` matches is therefore decided by the loxilb in the image, not by the Ingress specification:

| | `a.example.com` | `a.b.example.com` | `example.com` |
| --- | --- | --- | --- |
| Kubernetes Ingress specification | matches | doesn't match | doesn't match |
| loxilb (Gateway API meaning) | matches | matches | doesn't match |

* With loxilb, a wildcard host matches more than one label, so an Ingress with `*.example.com` also receives requests for `a.b.example.com`.
* Wildcard hosts need a loxilb that supports them. The current base image (`loxilb-inference-gateway:v0.9.9.1`) doesn't route by a wildcard host and doesn't select a wildcard certificate by SNI.

## Backends in other namespaces

An Ingress refers to Services in its own namespace. To use a Service in another namespace, set `external-backend-service` and name the Service's namespace in `service-<Service name>-namespace`:

```
metadata:
  annotations:
    external-backend-service: "true"
    service-api-namespace: "backends"   # the Service "api" is in the namespace "backends"
```

kube-loxilb sets these on the Ingresses it makes from HTTPRoutes whose backends are in another namespace than the Ingress.
