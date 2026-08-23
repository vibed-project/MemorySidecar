# syntax=docker/dockerfile:1.7

# --- build stage --------------------------------------------------------------
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build

# TARGETOS / TARGETARCH are injected by BuildKit for each --platform.
# Declare them WITHOUT defaults: assigning a default shadows the value BuildKit
# injects, so every platform cross-compiles to that default and the linux/arm64
# manifest ends up holding amd64 binaries (verified: `make docker` on an arm64
# host produced an arm64-labelled image containing x86-64 executables).
ARG TARGETOS
ARG TARGETARCH

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
      -X github.com/vibed-project/mindD/internal/version.Version=${VERSION} \
      -X github.com/vibed-project/mindD/internal/version.Commit=${COMMIT} \
      -X github.com/vibed-project/mindD/internal/version.BuildDate=${BUILD_DATE}"; \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "${LDFLAGS}" -o /out/mindd ./cmd/mindd; \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "${LDFLAGS}" -o /out/mindctl    ./cmd/mindctl

# --- runtime stage ------------------------------------------------------------
FROM gcr.io/distroless/static:nonroot

# Distroless `static:nonroot` runs as UID/GID 65532. /tmp is writable for the
# UDS socket; configs are expected at /etc/mindd/.
WORKDIR /

COPY --from=build /out/mindd /usr/local/bin/mindd
COPY --from=build /out/mindctl    /usr/local/bin/mindctl

EXPOSE 7777 8080 9090

ENTRYPOINT ["/usr/local/bin/mindd"]
CMD ["--config", "/etc/mindd/config.yaml"]
