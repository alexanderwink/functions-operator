# Shared runtime image: funcrt (Lambda Runtime API emulator / supervisor) + fetcher (S3 downloader).
# Build from the repo root: podman build -f images/runtime/Dockerfile .
ARG BASE_IMAGE=golang:1.26
FROM --platform=$BUILDPLATFORM ${BASE_IMAGE} AS builder
ARG TARGETOS
ARG TARGETARCH
WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
COPY api/ api/
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -trimpath -o /out/funcrt ./cmd/funcrt && \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -trimpath -o /out/fetcher ./cmd/fetcher

# amazonlinux:2023-minimal matches the Lambda provided.al2023 userland (glibc, libstdc++, icu for .NET AOT).
FROM public.ecr.aws/amazonlinux/amazonlinux:2023-minimal
COPY --from=builder /out/funcrt /usr/local/bin/funcrt
COPY --from=builder /out/fetcher /usr/local/bin/fetcher
ENTRYPOINT ["/usr/local/bin/funcrt"]
