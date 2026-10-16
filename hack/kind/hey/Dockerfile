# Load generator for the kind e2e suite. Published hey images (williamyeh/hey etc.) are amd64-only,
# so build rakyll/hey for the native arch and load it into kind (make hey-image-kind).
ARG BASE_IMAGE=golang:1.26
FROM ${BASE_IMAGE} AS builder
RUN CGO_ENABLED=0 GOBIN=/out go install github.com/rakyll/hey@v0.1.5

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /out/hey /usr/local/bin/hey
ENTRYPOINT ["/usr/local/bin/hey"]
