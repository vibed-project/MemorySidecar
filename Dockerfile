# syntax=docker/dockerfile:1.7

# --- build stage --------------------------------------------------------------
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build

# TARGETOS / TARGETARCH are set automatically by `docker buildx build`.
ARG TARGETOS=linux
ARG TARGETARCH=amd64

WORKDIR /src

# Cache module downloads in a layer separate from the source.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown

# Statically-linked binaries so the runtime image can be distroless.
ENV CGO_ENABLED=0
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    set -eux; \
    LDFLAGS="-s -w \
      -X memsidecar/internal/version.Version=${VERSION} \
      -X memsidecar/internal/version.Commit=${COMMIT} \
      -X memsidecar/internal/version.BuildDate=${BUILD_DATE}"; \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "${LDFLAGS}" -o /out/memsidecar ./cmd/memsidecar; \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "${LDFLAGS}" -o /out/memctl    ./cmd/memctl

# --- runtime stage ------------------------------------------------------------
FROM gcr.io/distroless/static:nonroot

# Distroless `static:nonroot` runs as UID/GID 65532. /tmp is writable for the
# UDS socket; configs are expected at /etc/memsidecar/.
WORKDIR /

COPY --from=build /out/memsidecar /usr/local/bin/memsidecar
COPY --from=build /out/memctl    /usr/local/bin/memctl

EXPOSE 7777 8080 9090

ENTRYPOINT ["/usr/local/bin/memsidecar"]
CMD ["--config", "/etc/memsidecar/config.yaml"]
