# loxilb-ingress needs the SNI certificate API (/netlox/v1/sni/certificates) and mTLS support.
# loxilb-inference-gateway has both; the open-source loxilb image has neither.
# TEST BUILD, not for merging: the base is loxilb-inference-gateway main (gateway 6f8ab04f),
# pinned by digest because its "latest" tag moves with every push to main.
ARG LOXILB_IMG="ghcr.io/loxilb-io/loxilb-inference-gateway@sha256:d0910127f6860efc1a8c6f9c3d78524a131557f5aa6f31d04784bd6507de912d"
FROM golang:1.23-alpine AS builder

RUN apk update && apk add git && apk add make

WORKDIR /usr/src/app
COPY . .

RUN make build

FROM $LOXILB_IMG

ARG GIT_VERSION="v0.1.0"
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
