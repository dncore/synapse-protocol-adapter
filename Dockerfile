# syntax=docker/dockerfile:1

# ---- build stage ----
FROM golang:1.27-alpine AS build
WORKDIR /src

# Overridable for restricted networks:
#   docker build --build-arg GOPROXY=https://goproxy.cn,direct .
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY}

# Cache dependencies first.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/synapse ./cmd/proxy

# ---- runtime stage ----
# distroless/static: no shell, no package manager, CA certs included,
# non-root by default via the `nonroot` user.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/synapse /synapse

# Config lives at /etc/synapse/config.yaml (mount read-only).
ENV PROXY_SERVER_LISTEN=0.0.0.0:8787

EXPOSE 8787

# distroless ships no curl/wget; the binary checks its own health endpoint.
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD ["/synapse", "healthcheck"]

USER nonroot:nonroot

ENTRYPOINT ["/synapse"]
CMD ["--config", "/etc/synapse/config.yaml"]
