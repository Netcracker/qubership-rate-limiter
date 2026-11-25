# The service image, built from the repository root: the service's tree
# imports the root module's internal packages and the engine module, so the
# context is the whole repository and the Dockerfile lives here so it is
# found beside the code it builds.
#
#   docker build -f service/Dockerfile .

FROM --platform=$BUILDPLATFORM golang:1.27@sha256:5f1b98fa02fe396ebce2a0227a0e08309e11792915b9dd2e1f71053d7e7e6666 AS builder
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

FROM ghcr.io/netcracker/qubership-core-base:alpine-2.6.1@sha256:7be670fc57c243195c63597106413e1b0365381c2d186a71cb6d12fcffc2278b
WORKDIR /app
COPY --chown=10001:0 --chmod=555 --from=builder /app/rate-limiter /app/rate-limiter
COPY --chown=10001:0 --chmod=444 --from=builder /app/service/application.yaml /app/application.yaml
USER 10001:10001

CMD ["/app/rate-limiter"]
