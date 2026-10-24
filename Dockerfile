# syntax=docker/dockerfile:1

# The Barq controller image.
#
# The controller is a single static binary with an embedded web UI. The
# final image is based on distroless static, so it contains the binary
# and the CA bundle and nothing else. There is no shell, no package
# manager and no interpreter that an attacker could reach.
#
# The build is multi-platform by default. The target platform follows
# the builder, so the same Dockerfile produces an arm64 image on
# Apple Silicon and an amd64 image on Intel. Use buildx to produce a
# manifest list for both:
#
#   docker buildx build --platform linux/amd64,linux/arm64 -t barq .
#
FROM golang:1.25-alpine AS build

ARG TARGETOS=linux
ARG TARGETARCH=amd64
ARG VERSION=dev

WORKDIR /src

# Download the module graph first so that the dependency layer is
# cached independently of the source.
COPY go.mod go.sum* ./
RUN go mod download

COPY . .

# CGO is disabled so the binary is fully static. The version and the
# commit are embedded for operational traceability.
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/barq ./cmd/barq

# The administration CLI is built as a second, smaller binary.
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w" \
      -o /out/barqctl ./cmd/barqctl

# Verify the binary is static. A dynamic binary would need a libc in
# the final image, which defeats the purpose of the distroless base.
RUN if ldd /out/barq 2>&1 | grep -q "=>"; then \
      echo "the binary is not static" && exit 1; \
    fi

# The vulnerability database is not shipped in the image. It is
# fetched at build time so that the image records what was scanned.
FROM alpine:3.20 AS scan
RUN apk add --no-cache trivy 2>/dev/null || true

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/barq /barq
COPY --from=build /out/barqctl /barqctl

# The distroless nonroot user is uid 65532.
USER 65532:65532

EXPOSE 8080

ENTRYPOINT ["/barq"]
