# The service image, built from the repository root: the service's tree
# imports the root module's internal packages and the engine module, so the
# context is the whole repository and the Dockerfile lives here so it is
# found beside the code it builds.
#
#   docker build -f service/Dockerfile .

FROM --platform=$BUILDPLATFORM golang:1.27@sha256:b76020929b709ddb6202afc05410a0faefae20ae05e27ad548b3870092688a69 AS builder
ARG TARGETOS
ARG TARGETARCH
# The build's own version stamp, and the fallback only: the pipeline passes
# no build arguments, so ratelimit_build_info reports SERVICE_VERSION from
# the container's environment, which the chart sets.
ARG VERSION=dev
# Allow Go toolchain auto-download when the image patch (e.g. 1.26.1) is
# older than the minimum declared in go.mod (e.g. 1.26.5).
ENV GOTOOLCHAIN=auto

WORKDIR /app
COPY go.mod go.mod
COPY go.sum go.sum
# The engine module is replace-directed to ./engine; its go.mod must be
# present before go mod download.
COPY engine/go.mod engine/go.mod
RUN go mod download

COPY . .

# The builder stage always runs on the build host platform (BUILDPLATFORM) to
# avoid QEMU emulation; Go cross-compiles through TARGETOS/TARGETARCH.
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS="${TARGETOS:-linux}" GOARCH="${TARGETARCH}" \
    go build -ldflags "-X main.version=${VERSION}" -o rate-limiter ./service/cmd/

# The binary carries no Kubernetes client, and the image proves it: the same
# check CI runs, so an image that leaks one is never pushed.
RUN go version -m rate-limiter | grep -E '^\s+dep\s+(k8s.io/client-go|sigs.k8s.io/controller-runtime)\s' \
    && { echo "the service binary depends on a Kubernetes client"; exit 1; } || true

FROM ghcr.io/netcracker/qubership-core-base:alpine-2.5.9@sha256:ab860790e4ecce56f8df673495cd5745ce587fffa7d7eb210d6d098563ccb26d
WORKDIR /app
COPY --chown=10001:0 --chmod=555 --from=builder /app/rate-limiter /app/rate-limiter
COPY --chown=10001:0 --chmod=444 --from=builder /app/service/application.yaml /app/application.yaml
USER 10001:10001

CMD ["/app/rate-limiter"]
