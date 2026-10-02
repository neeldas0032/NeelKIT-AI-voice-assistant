# NeelKIT server — CPU image.
#
# Multi-stage: the build needs a Go toolchain, a C compiler and the ONNX Runtime
# headers; the runtime needs none of those. Models are NOT baked into the image.
# They are 670 MB, they change independently of the code, and embedding them
# would make every rebuild a 670 MB push. They live on a named volume instead,
# fetched once by the models service in docker-compose.

ARG GO_VERSION=1.24
ARG ORT_VERSION=1.25.1

# ---------------------------------------------------------------- builder
FROM golang:${GO_VERSION}-bookworm AS builder
ARG ORT_VERSION
ARG TARGETARCH

RUN set -eux; \
    arch="$([ "${TARGETARCH}" = "arm64" ] && echo aarch64 || echo x64)"; \
    curl -fsSL -o /tmp/ort.tgz \
      "https://github.com/microsoft/onnxruntime/releases/download/v${ORT_VERSION}/onnxruntime-linux-${arch}-${ORT_VERSION}.tgz"; \
    tar -xzf /tmp/ort.tgz -C /opt; \
    mv "/opt/onnxruntime-linux-${arch}-${ORT_VERSION}" /opt/onnxruntime; \
    rm /tmp/ort.tgz

ENV CGO_ENABLED=1 \
    CGO_CFLAGS="-I/opt/onnxruntime/include" \
    CGO_LDFLAGS="-L/opt/onnxruntime/lib"

WORKDIR /src

# Dependencies are copied first so a source-only change reuses the module cache.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=unknown
RUN set -eux; \
    BUILD_DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"; \
    go build \
      -tags sqlite_fts5 \
      -trimpath \
      -ldflags "-s -w \
        -X github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/version.Version=${VERSION} \
        -X github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/version.Commit=${COMMIT} \
        -X github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/version.BuildDate=${BUILD_DATE}" \
      -o /out/neelkitd ./cmd/neelkitd; \
    go build -tags sqlite_fts5 -trimpath -ldflags "-s -w" -o /out/nk ./cmd/nk

# ---------------------------------------------------------------- runtime
FROM debian:bookworm-slim AS runtime
ARG ORT_VERSION
ARG TARGETARCH

RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
      ca-certificates curl ffmpeg git; \
    arch="$([ "${TARGETARCH}" = "arm64" ] && echo aarch64 || echo x64)"; \
    curl -fsSL -o /tmp/ort.tgz \
      "https://github.com/microsoft/onnxruntime/releases/download/v${ORT_VERSION}/onnxruntime-linux-${arch}-${ORT_VERSION}.tgz"; \
    tar -xzf /tmp/ort.tgz -C /opt; \
    mv "/opt/onnxruntime-linux-${arch}-${ORT_VERSION}" /opt/onnxruntime; \
    rm /tmp/ort.tgz; \
    apt-get purge -y curl; \
    apt-get autoremove -y; \
    rm -rf /var/lib/apt/lists/*

ENV LD_LIBRARY_PATH=/opt/onnxruntime/lib \
    NEELKIT_ORT_LIBRARY=/opt/onnxruntime/lib/libonnxruntime.so \
    NEELKIT_MODELS_DIR=/models \
    NEELKIT_DATA_DIR=/data \
    NEELKIT_WORKSPACE_ROOT=/workspace \
    NEELKIT_HTTP_ADDR=0.0.0.0:8420

# The server can run programs in the workspace, so it never runs as root.
RUN useradd --uid 10001 --create-home --shell /usr/sbin/nologin neelkit \
    && mkdir -p /models /data /workspace \
    && chown -R neelkit:neelkit /data /workspace

COPY --from=builder /out/neelkitd /usr/local/bin/neelkitd
COPY --from=builder /out/nk /usr/local/bin/nk

USER neelkit
WORKDIR /home/neelkit
EXPOSE 8420

# Liveness only. Readiness is /readyz and is checked by the compose dependency,
# because a container that is up but still loading a 670 MB model should not
# yet receive traffic.
HEALTHCHECK --interval=15s --timeout=3s --start-period=90s --retries=5 \
  CMD ["/usr/local/bin/nk", "doctor", "--quiet"]

ENTRYPOINT ["/usr/local/bin/neelkitd"]
