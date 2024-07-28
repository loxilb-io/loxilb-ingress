FROM golang:1.22-alpine AS builder

RUN apk update && apk add git && apk add make

WORKDIR /usr/src/app
COPY . .

RUN make build

FROM ghcr.io/loxilb-io/loxilb:latest

LABEL name="loxilb-ingress-manager" \
      vendor="loxilb.io" \
      version=$GIT_VERSION \
      release="0.1" \
      summary="loxilb-ingress-manager docker image" \
      description="ingress implementation for loxilb" \
      maintainer="backguyn@netlox.io"

WORKDIR /bin/
COPY --from=builder /usr/src/app/bin/loxilb-ingress /bin/loxilb-ingress

USER root
RUN chmod +x /bin/loxilb-ingress
