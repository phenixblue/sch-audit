# Build the manager binary
FROM golang:1.26 AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go mod download

# Copy the Go source (relies on .dockerignore to filter)
COPY . .

# Build
# the GOARCH has no default value to allow the binary to be built according to the host where the command
# was called. For example, if we call make docker-build in a local env which has the Apple Silicon M1 SO
# the docker BUILDPLATFORM arg will be linux/arm64 when for Apple x86 it will be linux/amd64. Therefore,
# by leaving it empty we can ensure that the container and binary shipped on it will have the same platform.
#
# Built by package directory (./cmd/...), not by listing the main.go file
# directly: some build environments (e.g. OpenShift's binary Docker builds)
# don't reliably run `go build` in module mode for a bare file-path
# argument, and mis-resolve "cmd/manager/main.go" as a standard-library
# import path ("package cmd/manager/main.go is not in std") instead of a
# source file. The ./cmd/... form is unambiguous everywhere.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o manager ./cmd/manager
# sweep is the retention CronJob's one-shot binary (see
# config/manager/sweep_cronjob.yaml); it ships in the same image as manager
# so there's one build/publish pipeline for the whole project.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o sweep ./cmd/sweep
# dashboard serves the read-only web UI (config/manager/dashboard.yaml); its
# static assets are compiled in via go:embed, so nothing else needs copying.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o dashboard ./cmd/dashboard
# extender is the optional scheduler-Extender observer (config/manager/
# extender.yaml); not wired into any scheduler by default - see
# docs/tier2-investigation.md and cmd/extender's package doc.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o extender ./cmd/extender

# Use distroless as minimal base image to package the manager binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/manager .
COPY --from=builder /workspace/sweep .
COPY --from=builder /workspace/dashboard .
COPY --from=builder /workspace/extender .
USER 65532:65532

ENTRYPOINT ["/manager"]
