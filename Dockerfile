# syntax=docker/dockerfile:1

# Build stage: runs on the build host, cross-compiles for the target platform.
FROM --platform=$BUILDPLATFORM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG TARGETOS=linux
ARG TARGETARCH
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION" \
    -o /out/container-image-exporter ./cmd/exporter

# Runtime stage: static binary, CA certificates, no shell, non-root.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/container-image-exporter /container-image-exporter
# Keep ECR tokens in memory only; the root filesystem is meant to be read-only.
ENV AWS_ECR_DISABLE_CACHE=true
USER 65532:65532
EXPOSE 9877
ENTRYPOINT ["/container-image-exporter"]
