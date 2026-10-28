FROM codeberg.org/gigirassy/thc:latest AS thc

FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /src
RUN apk add --no-cache ca-certificates
COPY go.mod main.go ./

RUN CGO_ENABLED=0 \
    GOOS=${TARGETOS:-linux} \
    GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath \
      -ldflags="-s -w -extldflags '-static' -buildid=''" \
      -o /pinata ./main.go


FROM scratch AS runtime

USER 1000:1000

COPY --from=builder /pinata /pinata
COPY --from=thc /thc /thc
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt

ENV GOMEMLIMIT=15MiB \
    GOGC=20 \
    THC_PORT_NAME=THC_PORT \
    THC_PORT=8080 \
    THC_PATH=/

EXPOSE 8080

ENTRYPOINT ["/pinata"]

HEALTHCHECK --interval=5s --timeout=5s --start-period=10s --retries=5 CMD ["/thc"]
