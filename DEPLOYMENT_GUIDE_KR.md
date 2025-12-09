# loxilb-ingress 배포 및 설정 가이드

이 문서는 loxilb-ingress를 Kubernetes 클러스터에 배포하고 설정하는 방법을 자세히 설명합니다.

## 목차
- [사전 요구사항](#사전-요구사항)
- [배포 절차](#배포-절차)
  - [1. LoxiLB 설치](#1-loxilb-설치)
  - [2. TLS 인증서 준비](#2-tls-인증서-준비)
  - [3. loxilb-ingress 배포](#3-loxilb-ingress-배포)
  - [4. LoadBalancer Service 생성](#4-loadbalancer-service-생성)
  - [5. Ingress 리소스 생성](#5-ingress-리소스-생성)
- [Ingress 설정 상세](#ingress-설정-상세)
  - [IngressClass 설정](#ingressclass-설정)
  - [TLS/HTTPS 설정](#tlshttps-설정)
  - [Annotations 설정](#annotations-설정)
  - [PathType 설정](#pathtype-설정)
- [고급 설정](#고급-설정)
  - [Direct LoadBalancing 모드](#direct-loadbalancing-모드)
  - [Endpoint 선택 알고리즘](#endpoint-선택-알고리즘)
  - [외부 백엔드 서비스](#외부-백엔드-서비스)
- [문제 해결](#문제-해결)

---

## 사전 요구사항

- Kubernetes 클러스터 (v1.19 이상 권장)
- kubectl CLI 도구
- LoxiLB 또는 호환 가능한 LoadBalancer 솔루션
- (옵션) TLS 인증서 생성 도구 (OpenSSL, Minica 등)

---

## 배포 절차

### 1. LoxiLB 설치

loxilb-ingress는 L4 LoadBalancer로 LoxiLB를 사용합니다. 먼저 LoxiLB를 설치하세요.

LoxiLB 설치 가이드는 [공식 문서](https://github.com/loxilb-io/loxilb)를 참고하세요.

설치 후 다음 명령으로 확인:
```bash
kubectl get pods -n kube-system | grep loxilb
```

### 2. TLS 인증서 준비

#### 2.1 인증서 생성

**OpenSSL 사용:**
```bash
# Self-signed 인증서 생성
openssl req -x509 -nodes -days 365 -newkey rsa:2048 \
  -keyout server.key -out server.crt \
  -subj "/CN=*.loxilb.io/O=loxilb"
```

**Minica 사용:**
```bash
minica --domains='*.loxilb.io'
```

#### 2.2 Kubernetes Secret 생성

**방법 1: kubectl 명령 사용 (권장)**
```bash
kubectl create secret tls loxilb-ssl \
  --cert=server.crt \
  --key=server.key \
  -n kube-system
```

**방법 2: YAML 매니페스트 사용**
```bash
# Base64 인코딩
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

**방법 3: Opaque Secret 사용**

loxilb-ingress는 `kubernetes.io/tls` 타입 외에도 `Opaque` 타입 Secret을 지원합니다:

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

지원되는 키 이름:
- **인증서**: `tls.crt`, `cert`, `certificate`, `tls.cert`, `server.crt`
- **개인키**: `tls.key`, `key`, `private-key`, `privatekey`, `tls.private.key`, `server.key`

#### 2.3 Secret 확인
```bash
kubectl get secret loxilb-ssl -n kube-system
```

### 3. loxilb-ingress 배포

#### 3.1 매니페스트 적용
```bash
kubectl apply -f https://raw.githubusercontent.com/loxilb-io/loxilb-ingress/main/manifests/loxilb-ingress-deploy.yml
```

#### 3.2 loxilb-ingress 옵션 설정 (선택사항)

loxilb-ingress는 배포 매니페스트에서 설정할 수 있는 추가 명령줄 옵션을 지원합니다:

**사용 가능한 옵션:**

- `--proxyonlymode`: 내장된 LoxiLB를 프록시 전용 모드로 실행 (eBPF 가속 없는 L7 프록시)
- `--prometheus` 또는 `-p`: LoxiLB의 Prometheus 메트릭 수집 활성화

**예제: 배포에 옵션 추가:**

`loxilb-ingress-deploy.yml`의 DaemonSet을 편집합니다:

```yaml
spec:
  template:
    spec:
      containers:
      - name: loxilb-ingress
        image: "ghcr.io/loxilb-io/loxilb-ingress:latest"
        command: 
          - "/bin/loxilb-ingress"
          - "--proxyonlymode"      # 프록시 전용 모드 활성화
          - "--prometheus"          # Prometheus 메트릭 활성화
        # ... 나머지 설정
```

**옵션 조합:**

| 옵션 | LoxiLB 동작 |
|------|-------------|
| (없음) | 전체 eBPF 모드 |
| `--proxyonlymode` | 프록시 전용 모드 (eBPF 없는 L7) |
| `--prometheus` | 전체 모드 + Prometheus 메트릭 |
| `--proxyonlymode --prometheus` | 프록시 전용 모드 + Prometheus 메트릭 |

#### 3.3 배포 확인
```bash
# Pod 상태 확인
kubectl get pods -n kube-system -l app=loxilb-ingress

# DaemonSet 확인
kubectl get daemonset loxilb-ingress -n kube-system

# 로그 확인
kubectl logs -n kube-system -l app=loxilb-ingress
```

### 4. LoadBalancer Service 생성

loxilb-ingress를 외부에 노출하기 위한 LoadBalancer Service를 생성합니다:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: loxilb-ingress-manager
  namespace: kube-system
  annotations:
    loxilb.io/lbmode: "onearm"  # LoxiLB 모드 설정
spec:
  type: LoadBalancer
  loadBalancerClass: loxilb.io/loxilb
  externalTrafficPolicy: Local  # 클라이언트 IP 보존
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

**적용:**
```bash
kubectl apply -f manifests/loxilb-ingress-lb.yml
```

**External IP 확인:**
```bash
kubectl get svc loxilb-ingress-manager -n kube-system

# 출력 예시:
# NAME                     TYPE           EXTERNAL-IP        PORT(S)
# loxilb-ingress-manager   LoadBalancer   llb-192.168.80.9   80:31686/TCP,443:31994/TCP
```

이 EXTERNAL-IP가 모든 Ingress 리소스에서 사용되는 진입점이 됩니다.

### 5. Ingress 리소스 생성

#### 5.1 백엔드 애플리케이션 배포

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

#### 5.2 Ingress 리소스 생성

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: myapp-ingress
  namespace: default
spec:
  ingressClassName: loxilb  # 필수: loxilb-ingress 사용 지정
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

**적용 및 확인:**
```bash
kubectl apply -f myapp-ingress.yaml
kubectl get ingress myapp-ingress
```

---

## Ingress 설정 상세

### IngressClass 설정

loxilb-ingress가 Ingress 리소스를 관리하려면 **반드시** `ingressClassName`을 `loxilb`로 설정해야 합니다.

```yaml
spec:
  ingressClassName: loxilb  # 필수!
```

이 설정이 없거나 다른 값으로 설정된 Ingress는 loxilb-ingress가 처리하지 않습니다.

### TLS/HTTPS 설정

#### 기본 TLS 설정

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
    secretName: loxilb-ssl  # TLS Secret 이름
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

#### 동작 방식

1. **TLS가 설정된 경우:**
   - 클라이언트는 HTTPS (443 포트)로 연결
   - loxilb-ingress가 TLS 종료 수행
   - 백엔드 서비스로는 HTTP로 전달
   - Secret의 인증서는 `/opt/loxilb/cert/<hostname>/` 디렉토리에 저장됨

2. **TLS가 없는 경우:**
   - 클라이언트는 HTTP (80 포트)로 연결
   - 평문 HTTP 통신

#### 여러 호스트에 대한 TLS

```yaml
spec:
  ingressClassName: loxilb
  tls:
  - hosts:
    - app1.example.com
    - app2.example.com
    secretName: wildcard-cert  # 와일드카드 인증서
  - hosts:
    - app3.example.com
    secretName: app3-cert  # 개별 인증서
  rules:
  - host: app1.example.com
    # ...
  - host: app2.example.com
    # ...
  - host: app3.example.com
    # ...
```

### Annotations 설정

loxilb-ingress는 다양한 Annotations를 통해 세부 동작을 제어할 수 있습니다.

#### 1. Endpoint 선택 알고리즘

**Annotation:** `loxilb.io/epselect`

로드밸런싱 알고리즘을 지정합니다.

```yaml
metadata:
  annotations:
    loxilb.io/epselect: "rr"  # Round-Robin (기본값)
```

**지원 값:**
- `rr`: Round-Robin (기본값) - 순차적으로 분산
- `hash`: Hash 기반 - 클라이언트 IP/포트 기반 해시
- `lc`: Least Connections - 가장 적은 연결 수를 가진 엔드포인트
- `persist`: Persistent Round-Robin - 세션 지속성 유지
- `priority`: Priority 기반 - 우선순위 기반 선택
- `n2`: Power of Two Choices - 두 개 중 더 나은 것 선택

**예시:**
```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: myapp-ingress
  annotations:
    loxilb.io/epselect: "lc"  # Least Connections 사용
spec:
  ingressClassName: loxilb
  # ...
```

#### 2. Direct LoadBalancing 모드

일반적인 Ingress 대신 특정 Service를 직접 LoadBalancing합니다.

**Annotations:**
- `loxilb.io/direct-loadbalance-service`: 대상 Service 이름
- `loxilb.io/direct-loadbalance-namespace`: Service Namespace (옵션)

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
  # rules는 무시됨
```

**동작 특징:**
- HTTP/HTTPS 규칙 대신 Service의 모든 포트를 직접 노출
- TCP/UDP 프로토콜 지원
- Layer 4 로드밸런싱

#### 3. 외부 백엔드 서비스

다른 Namespace의 Service를 백엔드로 사용할 수 있습니다.

```yaml
metadata:
  annotations:
    external-backend-service: "true"
    service-myapp-service-namespace: "production"
```

이 경우 `myapp-service`는 `production` Namespace에서 검색됩니다.

#### 4. LoadBalancer Service 연동

Gateway API 또는 특정 LoadBalancer Service와 연동:

```yaml
metadata:
  annotations:
    loadbalancer-service: "my-lb-service"
    loadbalancer-service-namespace: "kube-system"
```

또는 Gateway API 사용:

```yaml
metadata:
  annotations:
    gateway-api-controller: "loxilb.io/loxilb"
    parent-gateway: "my-gateway"
    parent-gateway-namespace: "default"
```

### PathType 설정

Kubernetes Ingress의 표준 PathType을 지원합니다.

#### Prefix (가장 일반적)

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

**매칭 규칙:**
- `/app` → 매칭 ✓
- `/app/` → 매칭 ✓
- `/app/page` → 매칭 ✓
- `/application` → 매칭 ✗

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

**매칭 규칙:**
- `/api/v1/users` → 매칭 ✓
- `/api/v1/users/` → 매칭 ✗
- `/api/v1/users/123` → 매칭 ✗

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

구현체에 따라 동작이 다를 수 있습니다.

---

## 고급 설정

### Direct LoadBalancing 모드

일반 Ingress는 HTTP 레이어(L7)에서 동작하지만, Direct LoadBalancing 모드는 L4에서 동작합니다.

**일반 Ingress 모드:**
```
Client → LoxiLB (L4) → loxilb-ingress (L7 HTTP) → Backend Pod
```

**Direct LoadBalancing 모드:**
```
Client → LoxiLB (L4) → Backend Pod
```

**사용 예시:**

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
    loxilb.io/epselect: "hash"  # 클라이언트 IP 기반 분산
spec:
  ingressClassName: loxilb
```

**장점:**
- L4 프로토콜 지원 (TCP, UDP)
- 낮은 레이턴시
- HTTP가 아닌 프로토콜 지원

**제한사항:**
- Path 기반 라우팅 불가
- Host 기반 라우팅 불가
- HTTP 헤더 기반 라우팅 불가

### Endpoint 선택 알고리즘

각 알고리즘의 사용 시나리오:

#### Round-Robin (`rr`)
```yaml
annotations:
  loxilb.io/epselect: "rr"
```
- **적합한 경우:** 동일한 성능의 백엔드 서버
- **특징:** 가장 단순하고 공정한 분산
- **예시:** 상태가 없는(stateless) 웹 애플리케이션

#### Hash (`hash`)
```yaml
annotations:
  loxilb.io/epselect: "hash"
```
- **적합한 경우:** 세션 어피니티가 필요한 경우
- **특징:** 동일 클라이언트는 항상 같은 백엔드로 라우팅
- **예시:** 로컬 캐시를 사용하는 애플리케이션

#### Least Connections (`lc`)
```yaml
annotations:
  loxilb.io/epselect: "lc"
```
- **적합한 경우:** 요청 처리 시간이 다양한 경우
- **특징:** 현재 연결 수가 가장 적은 백엔드 선택
- **예시:** 장기 실행 요청이 있는 API 서버

#### Persistent (`persist`)
```yaml
annotations:
  loxilb.io/epselect: "persist"
```
- **적합한 경우:** 세션 지속성이 필요하지만 Hash보다 유연한 경우
- **특징:** Round-Robin + 세션 지속성
- **예시:** 쇼핑몰, 로그인 세션 유지

#### Priority (`priority`)
```yaml
annotations:
  loxilb.io/epselect: "priority"
```
- **적합한 경우:** 우선순위가 있는 백엔드 서버
- **특징:** 높은 우선순위 백엔드를 먼저 사용
- **예시:** Primary-Secondary 구성

#### N2 (Power of Two) (`n2`)
```yaml
annotations:
  loxilb.io/epselect: "n2"
```
- **적합한 경우:** 대규모 분산 시스템
- **특징:** 두 개의 무작위 백엔드 중 부하가 적은 것 선택
- **예시:** 마이크로서비스 아키텍처

### 외부 백엔드 서비스

여러 Namespace에 분산된 서비스를 하나의 Ingress로 관리:

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
            name: api-service  # api-ns 네임스페이스에서 검색
            port:
              number: 8080
  - host: web.example.com
    http:
      paths:
      - path: /
        pathType: Prefix
        backend:
          service:
            name: web-service  # web-ns 네임스페이스에서 검색
            port:
              number: 80
```

---

## 실전 예제

### 예제 1: 단일 도메인 HTTP/HTTPS

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

### 예제 2: 다중 도메인 라우팅

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

### 예제 3: Path 기반 라우팅

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

### 예제 4: TCP 서비스 Direct LoadBalancing

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

## 문제 해결

### loxilb-ingress Pod가 시작되지 않음

**확인 사항:**
```bash
# Pod 상태 확인
kubectl describe pod -n kube-system -l app=loxilb-ingress

# 로그 확인
kubectl logs -n kube-system -l app=loxilb-ingress
```

**일반적인 원인:**
1. Secret `loxilb-ssl`이 없음
   ```bash
   kubectl get secret loxilb-ssl -n kube-system
   ```
2. RBAC 권한 부족
   ```bash
   kubectl get clusterrolebinding loxilb-ingress
   ```

### Ingress가 트래픽을 받지 못함

**체크리스트:**

1. **IngressClassName 확인**
   ```bash
   kubectl get ingress <ingress-name> -o yaml | grep ingressClassName
   ```
   `loxilb`로 설정되어 있어야 합니다.

2. **LoadBalancer Service 확인**
   ```bash
   kubectl get svc loxilb-ingress-manager -n kube-system
   ```
   EXTERNAL-IP가 할당되어 있어야 합니다.

3. **Backend Service 및 Endpoints 확인**
   ```bash
   kubectl get endpoints <service-name>
   ```
   Endpoints가 존재해야 합니다.

4. **loxilb-ingress 로그 확인**
   ```bash
   kubectl logs -n kube-system -l app=loxilb-ingress --tail=100
   ```

### TLS/HTTPS가 작동하지 않음

**확인 사항:**

1. **Secret 확인**
   ```bash
   kubectl get secret <secret-name> -n <namespace>
   kubectl describe secret <secret-name> -n <namespace>
   ```

2. **Secret 데이터 확인**
   ```bash
   kubectl get secret <secret-name> -o jsonpath='{.data}' -n <namespace>
   ```
   `tls.crt`와 `tls.key` 또는 다른 지원 키가 있어야 합니다.

3. **인증서 파일 확인 (Pod 내부)**
   ```bash
   kubectl exec -n kube-system <loxilb-ingress-pod> -- ls -la /opt/loxilb/cert/
   ```

4. **LoxiLB API 확인**
   ```bash
   kubectl exec -n kube-system <loxilb-ingress-pod> -- \
     curl localhost:11111/netlox/v1/config/snicert/all
   ```

### Endpoint 선택이 예상대로 작동하지 않음

```bash
# LoxiLB 로드밸런서 규칙 확인
kubectl exec -n kube-system <loxilb-ingress-pod> -- \
  curl localhost:11111/netlox/v1/config/loadbalancer/all
```

예상되는 선택 알고리즘(`sel` 필드)과 엔드포인트가 올바른지 확인하세요.

### DNS 설정

**로컬 테스트:**
```bash
# /etc/hosts 편집
echo "192.168.80.9  myapp.example.com" | sudo tee -a /etc/hosts

# 테스트
curl http://myapp.example.com
curl -k https://myapp.example.com
```

**프로덕션 환경:**
- DNS A 레코드를 LoadBalancer의 EXTERNAL-IP로 설정
- 와일드카드 도메인 사용 시 `*.example.com` 레코드 생성

---

## 요약

### 필수 설정 체크리스트

- [ ] LoxiLB 설치 및 작동 확인
- [ ] TLS Secret 생성 (`loxilb-ssl`)
- [ ] loxilb-ingress DaemonSet 배포
- [ ] LoadBalancer Service 생성 및 EXTERNAL-IP 확인
- [ ] Ingress에 `ingressClassName: loxilb` 설정
- [ ] Backend Service 및 Endpoints 확인
- [ ] DNS 설정 (프로덕션 환경)

### 주요 Annotations

| Annotation | 값 | 설명 |
|-----------|-----|------|
| `loxilb.io/epselect` | `rr`, `hash`, `lc`, `persist`, `priority`, `n2` | 엔드포인트 선택 알고리즘 |
| `loxilb.io/direct-loadbalance-service` | Service 이름 | Direct L4 LoadBalancing 모드 |
| `loxilb.io/direct-loadbalance-namespace` | Namespace | Direct LB 대상 Namespace |
| `external-backend-service` | `"true"` | 외부 Namespace 백엔드 활성화 |
| `service-<name>-namespace` | Namespace | 특정 서비스의 Namespace |

### 지원 기능

- ✅ HTTP/HTTPS Ingress
- ✅ TLS 종료 (kubernetes.io/tls 및 Opaque Secret)
- ✅ 호스트 기반 라우팅
- ✅ Path 기반 라우팅
- ✅ 다양한 로드밸런싱 알고리즘
- ✅ Direct L4 LoadBalancing
- ✅ 외부 Namespace 백엔드
- ✅ eBPF 기반 고성능 처리

---

더 자세한 정보는 [GitHub 저장소](https://github.com/loxilb-io/loxilb-ingress)를 참고하세요.
