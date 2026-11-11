# The operator image, built from the repository root: the operator's tree
# imports the root module's internal packages and the engine module, so the
# context is the whole repository and the Dockerfile lives here so it is
# found beside the code it builds.
#
#   docker build -f operator/Dockerfile .

FROM --platform=$BUILDPLATFORM golang:1.27@sha256:01aa404fa56b604b6f48e90c1d6e2c79fe5cd78f9592f78d80303fbe171cdcc0 AS builder
ARG TARGETOS
ARG TARGETARCH
# The build's own version stamp, and the fallback only: the pipeline passes
# no build arguments, so the manifest's operatorVersion comes from
# OPERATOR_VERSION in the container's environment, which the chart sets.
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
    go build -ldflags "-X main.version=${VERSION}" -o rate-limiter-operator ./operator/cmd/

FROM ghcr.io/netcracker/qubership-core-base:alpine-2.5.13@sha256:891386ea91d90a1376e12f30b458fc23eb9793afd35f82817c90e4911cddd633
WORKDIR /app
COPY --chown=10001:0 --chmod=555 --from=builder /app/rate-limiter-operator /app/rate-limiter-operator
COPY --chown=10001:0 --chmod=444 --from=builder /app/operator/application.yaml /app/application.yaml
USER 10001:10001

CMD ["/app/rate-limiter-operator"]
