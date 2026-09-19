# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown

WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
# CGO is not needed: godbus speaks the D-Bus wire protocol directly and go-avahi sits on
# top of it, so there is no libdbus or libavahi-client to link against. That is what
# makes a static, distroless image possible.
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -tags timetzdata \
      -ldflags="-s -w \
        -X github.com/suprememoocow/espressif-exporter/internal/version.Version=${VERSION} \
        -X github.com/suprememoocow/espressif-exporter/internal/version.Commit=${COMMIT} \
        -X github.com/suprememoocow/espressif-exporter/internal/version.BuildDate=${BUILD_DATE}" \
      -o /out/espressif-exporter ./cmd/espressif-exporter

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/espressif-exporter /espressif-exporter

USER 65532:65532
EXPOSE 9826

# Distroless has no shell, so the check runs a subcommand of our own binary in exec form.
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
  CMD ["/espressif-exporter", "healthcheck"]

ENTRYPOINT ["/espressif-exporter"]
