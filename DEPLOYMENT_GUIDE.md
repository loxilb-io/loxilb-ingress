# loxilb-ingress Deployment and Configuration Guide

This document provides detailed instructions on how to deploy and configure loxilb-ingress in a Kubernetes cluster.

## Table of Contents
- [Prerequisites](#prerequisites)
- [Deployment Procedure](#deployment-procedure)
  - [1. Install LoxiLB](#1-install-loxilb)
  - [2. Prepare TLS Certificates](#2-prepare-tls-certificates)
  - [3. Deploy loxilb-ingress](#3-deploy-loxilb-ingress)
  - [4. Create LoadBalancer Service](#4-create-loadbalancer-service)
  - [5. Create Ingress Resources](#5-create-ingress-resources)
- [Ingress Configuration Details](#ingress-configuration-details)
  - [IngressClass Configuration](#ingressclass-configuration)
  - [TLS/HTTPS Configuration](#tlshttps-configuration)
  - [Annotations Configuration](#annotations-configuration)
  - [PathType Configuration](#pathtype-configuration)
- [Advanced Configuration](#advanced-configuration)
  - [Direct LoadBalancing Mode](#direct-loadbalancing-mode)
  - [Endpoint Selection Algorithms](#endpoint-selection-algorithms)
  - [External Backend Services](#external-backend-services)
- [Troubleshooting](#troubleshooting)

---

## Prerequisites

- Kubernetes cluster (v1.19 or higher recommended)
- kubectl CLI tool
- LoxiLB or compatible LoadBalancer solution
- (Optional) TLS certificate generation tools (OpenSSL, Minica, etc.)

---

## Deployment Procedure

### 1. Install LoxiLB

loxilb-ingress uses LoxiLB as an L4 LoadBalancer. Install LoxiLB first.

Refer to the [official documentation](https://github.com/loxilb-io/loxilb) for LoxiLB installation guide.

Verify installation with:
```bash
kubectl get pods -n kube-system | grep loxilb
```

### 2. Prepare TLS Certificates

#### 2.1 Generate Certificates

**Using OpenSSL:**
```bash
# Generate self-signed certificate
openssl req -x509 -nodes -days 365 -newkey rsa:2048 \
  -keyout server.key -out server.crt \
  -subj "/CN=*.loxilb.io/O=loxilb"
```

**Using Minica:**
```bash
minica --domains='*.loxilb.io'
```

#### 2.2 Create Kubernetes Secret

**Method 1: Using kubectl command (Recommended)**
```bash
kubectl create secret tls loxilb-ssl \
  --cert=server.crt \
  --key=server.key \
  -n kube-system
```

**Method 2: Using YAML manifest**
```bash
# Base64 encode
cat server.crt | base64 -w 0
cat server.key | base64 -w 0
```

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: loxilb-ssl
  namespace: kube-system
type: kubernetes.io/tls
data:
  tls.crt: <base64-encoded-certificate>
  tls.key: <base64-encoded-key>
```

**Method 3: Using Opaque Secret**

loxilb-ingress supports `Opaque` type secrets in addition to `kubernetes.io/tls`:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: loxilb-ssl
  namespace: kube-system
type: Opaque
data:
  cert: <base64-encoded-certificate>
  key: <base64-encoded-key>
```

Supported key names:
- **Certificate**: `tls.crt`, `cert`, `certificate`, `tls.cert`, `server.crt`
- **Private Key**: `tls.key`, `key`, `private-key`, `privatekey`, `tls.private.key`, `server.key`

#### 2.3 Verify Secret
```bash
kubectl get secret loxilb-ssl -n kube-system
```

### 3. Deploy loxilb-ingress

#### 3.1 Apply Manifest
```bash
kubectl apply -f https://raw.githubusercontent.com/loxilb-io/loxilb-ingress/main/manifests/loxilb-ingress-deploy.yml
```

#### 3.2 Configure loxilb-ingress Options (Optional)

loxilb-ingress supports additional command-line options that can be configured in the deployment manifest:

**Available Options:**

- `--proxyonlymode`: Run the embedded LoxiLB in proxy-only mode (Layer 7 proxy without eBPF acceleration)
- `--prometheus` or `-p`: Enable Prometheus metrics collection in LoxiLB

**Example: Adding options to the deployment:**

Edit the DaemonSet in `loxilb-ingress-deploy.yml`:

```yaml
spec:
  template:
    spec:
      containers:
      - name: loxilb-ingress
        image: "ghcr.io/loxilb-io/loxilb-ingress:latest"
        command: 
          - "/bin/loxilb-ingress"
          - "--proxyonlymode"      # Enable proxy-only mode
          - "--prometheus"          # Enable Prometheus metrics
        # ... rest of configuration
```

**Option Combinations:**

| Options | LoxiLB Behavior |
|---------|-----------------|
| (none) | Full eBPF mode |
| `--proxyonlymode` | Proxy-only mode (L7 without eBPF) |
| `--prometheus` | Full mode + Prometheus metrics |
| `--proxyonlymode --prometheus` | Proxy-only mode + Prometheus metrics |

#### 3.3 Verify Deployment
```bash
# Check pod status
kubectl get pods -n kube-system -l app=loxilb-ingress

# Check DaemonSet
kubectl get daemonset loxilb-ingress -n kube-system

# Check logs
kubectl logs -n kube-system -l app=loxilb-ingress
```

### 4. Create LoadBalancer Service

Create a LoadBalancer Service to expose loxilb-ingress externally:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: loxilb-ingress-manager
  namespace: kube-system
  annotations:
    loxilb.io/lbmode: "onearm"  # LoxiLB mode configuration
spec:
  type: LoadBalancer
  loadBalancerClass: loxilb.io/loxilb
  externalTrafficPolicy: Local  # Preserve client IP
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
```

**Apply:**
```bash
kubectl apply -f manifests/loxilb-ingress-lb.yml
```

**Check External IP:**
```bash
kubectl get svc loxilb-ingress-manager -n kube-system

# Example output:
# NAME                     TYPE           EXTERNAL-IP        PORT(S)
# loxilb-ingress-manager   LoadBalancer   llb-192.168.80.9   80:31686/TCP,443:31994/TCP
```

This EXTERNAL-IP becomes the entry point for all Ingress resources.

### 5. Create Ingress Resources

#### 5.1 Deploy Backend Application

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: myapp
  namespace: default
spec:
  replicas: 2
  selector:
    matchLabels:
      app: myapp
  template:
    metadata:
      labels:
        app: myapp
    spec:
      containers:
      - name: nginx
        image: nginx:latest
        ports:
        - containerPort: 80
---
apiVersion: v1
kind: Service
metadata:
  name: myapp-service
  namespace: default
spec:
  selector:
    app: myapp
  ports:
  - port: 80
    targetPort: 80
```

#### 5.2 Create Ingress Resource

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: myapp-ingress
  namespace: default
spec:
  ingressClassName: loxilb  # Required: specify loxilb-ingress usage
  rules:
  - host: myapp.example.com
    http:
      paths:
      - path: /
        pathType: Prefix
        backend:
          service:
            name: myapp-service
            port:
              number: 80
```

**Apply and verify:**
```bash
kubectl apply -f myapp-ingress.yaml
kubectl get ingress myapp-ingress
```

---

## Ingress Configuration Details

### IngressClass Configuration

For loxilb-ingress to manage an Ingress resource, **you must** set `ingressClassName` to `loxilb`.

```yaml
spec:
  ingressClassName: loxilb  # Required!
```

Ingress resources without this setting or with a different value will not be processed by loxilb-ingress.

### TLS/HTTPS Configuration

#### Basic TLS Configuration

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: secure-ingress
spec:
  ingressClassName: loxilb
  tls:
  - hosts:
    - secure.example.com
    secretName: loxilb-ssl  # TLS Secret name
  rules:
  - host: secure.example.com
    http:
      paths:
      - path: /
        pathType: Prefix
        backend:
          service:
            name: myapp-service
            port:
              number: 80
```

#### How it Works

1. **When TLS is configured:**
   - Clients connect via HTTPS (port 443)
   - loxilb-ingress performs TLS termination
   - Traffic is forwarded to backend services via HTTP
   - Secret certificates are stored in `/opt/loxilb/cert/<hostname>/` directory

2. **Without TLS:**
   - Clients connect via HTTP (port 80)
   - Plain HTTP communication

#### TLS for Multiple Hosts

```yaml
spec:
  ingressClassName: loxilb
  tls:
  - hosts:
    - app1.example.com
    - app2.example.com
    secretName: wildcard-cert  # Wildcard certificate
  - hosts:
    - app3.example.com
    secretName: app3-cert  # Individual certificate
  rules:
  - host: app1.example.com
    # ...
  - host: app2.example.com
    # ...
  - host: app3.example.com
    # ...
```

### Annotations Configuration

loxilb-ingress supports various annotations to control detailed behavior.

#### 1. Endpoint Selection Algorithm

**Annotation:** `loxilb.io/epselect`

Specifies the load balancing algorithm.

```yaml
metadata:
  annotations:
    loxilb.io/epselect: "rr"  # Round-Robin (default)
```

**Supported values:**
- `rr`: Round-Robin (default) - Sequential distribution
- `hash`: Hash-based - Based on client IP/port hash
- `lc`: Least Connections - Endpoint with fewest connections
- `persist`: Persistent Round-Robin - Maintains session persistence
- `priority`: Priority-based - Priority-based selection
- `n2`: Power of Two Choices - Chooses better of two random endpoints

**Example:**
```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: myapp-ingress
  annotations:
    loxilb.io/epselect: "lc"  # Use Least Connections
spec:
  ingressClassName: loxilb
  # ...
```

#### 2. Direct LoadBalancing Mode

Directly load balances a specific Service instead of using standard Ingress routing.

**Annotations:**
- `loxilb.io/direct-loadbalance-service`: Target Service name
- `loxilb.io/direct-loadbalance-namespace`: Service Namespace (optional)

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: direct-lb-ingress
  annotations:
    loxilb.io/direct-loadbalance-service: "myapp-service"
    loxilb.io/direct-loadbalance-namespace: "default"
    loxilb.io/epselect: "hash"
spec:
  ingressClassName: loxilb
  # rules are ignored
```

**Characteristics:**
- Exposes all ports of the Service directly instead of HTTP/HTTPS rules
- Supports TCP/UDP protocols
- Layer 4 load balancing

#### 3. External Backend Services

You can use Services from different Namespaces as backends.

```yaml
metadata:
  annotations:
    external-backend-service: "true"
    service-myapp-service-namespace: "production"
```

In this case, `myapp-service` will be searched in the `production` Namespace.

#### 4. LoadBalancer Service Integration

Integration with Gateway API or specific LoadBalancer Service:

```yaml
metadata:
  annotations:
    loadbalancer-service: "my-lb-service"
    loadbalancer-service-namespace: "kube-system"
```

Or using Gateway API:

```yaml
metadata:
  annotations:
    gateway-api-controller: "loxilb.io/loxilb"
    parent-gateway: "my-gateway"
    parent-gateway-namespace: "default"
```

### PathType Configuration

Supports standard Kubernetes Ingress PathTypes.

#### Prefix (Most Common)

```yaml
paths:
- path: /app
  pathType: Prefix
  backend:
    service:
      name: app-service
      port:
        number: 80
```

**Matching rules:**
- `/app` → Match ✓
- `/app/` → Match ✓
- `/app/page` → Match ✓
- `/application` → No match ✗

#### Exact

```yaml
paths:
- path: /api/v1/users
  pathType: Exact
  backend:
    service:
      name: api-service
      port:
        number: 8080
```

**Matching rules:**
- `/api/v1/users` → Match ✓
- `/api/v1/users/` → No match ✗
- `/api/v1/users/123` → No match ✗

#### ImplementationSpecific

```yaml
paths:
- path: /.*
  pathType: ImplementationSpecific
  backend:
    service:
      name: catch-all-service
      port:
        number: 80
```

Behavior may vary depending on implementation.

---

## Advanced Configuration

### Direct LoadBalancing Mode

While standard Ingress operates at HTTP layer (L7), Direct LoadBalancing mode operates at L4.

**Standard Ingress Mode:**
```
Client → LoxiLB (L4) → loxilb-ingress (L7 HTTP) → Backend Pod
```

**Direct LoadBalancing Mode:**
```
Client → LoxiLB (L4) → Backend Pod
```

**Usage Example:**

```yaml
apiVersion: v1
kind: Service
metadata:
  name: database-service
spec:
  selector:
    app: postgres
  ports:
  - name: postgres
    port: 5432
    targetPort: 5432
    protocol: TCP
---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: database-ingress
  annotations:
    loxilb.io/direct-loadbalance-service: "database-service"
    loxilb.io/epselect: "hash"  # Client IP-based distribution
spec:
  ingressClassName: loxilb
```

**Advantages:**
- L4 protocol support (TCP, UDP)
- Lower latency
- Support for non-HTTP protocols

**Limitations:**
- No path-based routing
- No host-based routing
- No HTTP header-based routing

### Endpoint Selection Algorithms

Use cases for each algorithm:

#### Round-Robin (`rr`)
```yaml
annotations:
  loxilb.io/epselect: "rr"
```
- **Best for:** Backend servers with equal performance
- **Characteristics:** Simplest and most fair distribution
- **Example:** Stateless web applications

#### Hash (`hash`)
```yaml
annotations:
  loxilb.io/epselect: "hash"
```
- **Best for:** When session affinity is needed
- **Characteristics:** Same client always routes to same backend
- **Example:** Applications using local cache

#### Least Connections (`lc`)
```yaml
annotations:
  loxilb.io/epselect: "lc"
```
- **Best for:** Variable request processing times
- **Characteristics:** Selects backend with fewest current connections
- **Example:** API servers with long-running requests

#### Persistent (`persist`)
```yaml
annotations:
  loxilb.io/epselect: "persist"
```
- **Best for:** Session persistence needed but more flexible than Hash
- **Characteristics:** Round-Robin + session persistence
- **Example:** Shopping sites, login session maintenance

#### Priority (`priority`)
```yaml
annotations:
  loxilb.io/epselect: "priority"
```
- **Best for:** Backend servers with priority levels
- **Characteristics:** Higher priority backends used first
- **Example:** Primary-Secondary configuration

#### N2 (Power of Two) (`n2`)
```yaml
annotations:
  loxilb.io/epselect: "n2"
```
- **Best for:** Large-scale distributed systems
- **Characteristics:** Chooses less loaded of two random backends
- **Example:** Microservices architecture

### External Backend Services

Managing services distributed across multiple Namespaces with a single Ingress:

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: multi-namespace-ingress
  namespace: default
  annotations:
    external-backend-service: "true"
    service-api-service-namespace: "api-ns"
    service-web-service-namespace: "web-ns"
spec:
  ingressClassName: loxilb
  rules:
  - host: api.example.com
    http:
      paths:
      - path: /
        pathType: Prefix
        backend:
          service:
            name: api-service  # Searched in api-ns namespace
            port:
              number: 8080
  - host: web.example.com
    http:
      paths:
      - path: /
        pathType: Prefix
        backend:
          service:
            name: web-service  # Searched in web-ns namespace
            port:
              number: 80
```

---

## Practical Examples

### Example 1: Single Domain HTTP/HTTPS

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: simple-ingress
spec:
  ingressClassName: loxilb
  tls:
  - hosts:
    - www.example.com
    secretName: example-tls
  rules:
  - host: www.example.com
    http:
      paths:
      - path: /
        pathType: Prefix
        backend:
          service:
            name: web-service
            port:
              number: 80
```

### Example 2: Multi-Domain Routing

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: multi-domain-ingress
  annotations:
    loxilb.io/epselect: "lc"
spec:
  ingressClassName: loxilb
  tls:
  - hosts:
    - app1.example.com
    - app2.example.com
    secretName: wildcard-tls
  rules:
  - host: app1.example.com
    http:
      paths:
      - path: /
        pathType: Prefix
        backend:
          service:
            name: app1-service
            port:
              number: 8080
  - host: app2.example.com
    http:
      paths:
      - path: /
        pathType: Prefix
        backend:
          service:
            name: app2-service
            port:
              number: 8080
```

### Example 3: Path-Based Routing

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: path-based-ingress
spec:
  ingressClassName: loxilb
  rules:
  - host: example.com
    http:
      paths:
      - path: /api
        pathType: Prefix
        backend:
          service:
            name: api-service
            port:
              number: 8080
      - path: /admin
        pathType: Prefix
        backend:
          service:
            name: admin-service
            port:
              number: 9090
      - path: /
        pathType: Prefix
        backend:
          service:
            name: frontend-service
            port:
              number: 80
```

### Example 4: TCP Service Direct LoadBalancing

```yaml
apiVersion: v1
kind: Service
metadata:
  name: redis-service
spec:
  selector:
    app: redis
  ports:
  - name: redis
    port: 6379
    targetPort: 6379
---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: redis-ingress
  annotations:
    loxilb.io/direct-loadbalance-service: "redis-service"
    loxilb.io/epselect: "hash"
spec:
  ingressClassName: loxilb
```

---

## Troubleshooting

### loxilb-ingress Pod Not Starting

**Check:**
```bash
# Check pod status
kubectl describe pod -n kube-system -l app=loxilb-ingress

# Check logs
kubectl logs -n kube-system -l app=loxilb-ingress
```

**Common causes:**
1. Secret `loxilb-ssl` not found
   ```bash
   kubectl get secret loxilb-ssl -n kube-system
   ```
2. Insufficient RBAC permissions
   ```bash
   kubectl get clusterrolebinding loxilb-ingress
   ```

### Ingress Not Receiving Traffic

**Checklist:**

1. **Verify IngressClassName**
   ```bash
   kubectl get ingress <ingress-name> -o yaml | grep ingressClassName
   ```
   Must be set to `loxilb`.

2. **Verify LoadBalancer Service**
   ```bash
   kubectl get svc loxilb-ingress-manager -n kube-system
   ```
   EXTERNAL-IP must be assigned.

3. **Verify Backend Service and Endpoints**
   ```bash
   kubectl get endpoints <service-name>
   ```
   Endpoints must exist.

4. **Check loxilb-ingress logs**
   ```bash
   kubectl logs -n kube-system -l app=loxilb-ingress --tail=100
   ```

### TLS/HTTPS Not Working

**Check:**

1. **Verify Secret**
   ```bash
   kubectl get secret <secret-name> -n <namespace>
   kubectl describe secret <secret-name> -n <namespace>
   ```

2. **Verify Secret Data**
   ```bash
   kubectl get secret <secret-name> -o jsonpath='{.data}' -n <namespace>
   ```
   Must have `tls.crt` and `tls.key` or other supported keys.

3. **Verify Certificate Files (Inside Pod)**
   ```bash
   kubectl exec -n kube-system <loxilb-ingress-pod> -- ls -la /opt/loxilb/cert/
   ```

4. **Check LoxiLB API**
   ```bash
   kubectl exec -n kube-system <loxilb-ingress-pod> -- \
     curl localhost:11111/netlox/v1/config/snicert/all
   ```

### Endpoint Selection Not Working as Expected

```bash
# Check LoxiLB load balancer rules
kubectl exec -n kube-system <loxilb-ingress-pod> -- \
  curl localhost:11111/netlox/v1/config/loadbalancer/all
```

Verify the expected selection algorithm (`sel` field) and endpoints are correct.

### DNS Configuration

**Local testing:**
```bash
# Edit /etc/hosts
echo "192.168.80.9  myapp.example.com" | sudo tee -a /etc/hosts

# Test
curl http://myapp.example.com
curl -k https://myapp.example.com
```

**Production environment:**
- Configure DNS A record to point to LoadBalancer's EXTERNAL-IP
- For wildcard domains, create `*.example.com` record

---

## Summary

### Essential Configuration Checklist

- [ ] Install and verify LoxiLB operation
- [ ] Create TLS Secret (`loxilb-ssl`)
- [ ] Deploy loxilb-ingress DaemonSet
- [ ] Create LoadBalancer Service and verify EXTERNAL-IP
- [ ] Set `ingressClassName: loxilb` in Ingress
- [ ] Verify Backend Service and Endpoints
- [ ] Configure DNS (production environment)

### Key Annotations

| Annotation | Values | Description |
|-----------|--------|-------------|
| `loxilb.io/epselect` | `rr`, `hash`, `lc`, `persist`, `priority`, `n2` | Endpoint selection algorithm |
| `loxilb.io/direct-loadbalance-service` | Service name | Direct L4 LoadBalancing mode |
| `loxilb.io/direct-loadbalance-namespace` | Namespace | Direct LB target Namespace |
| `external-backend-service` | `"true"` | Enable external Namespace backends |
| `service-<name>-namespace` | Namespace | Namespace for specific service |

### Supported Features

- ✅ HTTP/HTTPS Ingress
- ✅ TLS termination (kubernetes.io/tls and Opaque Secrets)
- ✅ Host-based routing
- ✅ Path-based routing
- ✅ Various load balancing algorithms
- ✅ Direct L4 LoadBalancing
- ✅ External Namespace backends
- ✅ eBPF-based high performance processing

---

For more information, refer to the [GitHub repository](https://github.com/loxilb-io/loxilb-ingress).
