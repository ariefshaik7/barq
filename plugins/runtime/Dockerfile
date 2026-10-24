# syntax=docker/dockerfile:1

# The Barq plugin runtime image.
#
# This image provides the toolchain that the worker contract requires:
# a POSIX shell, tar, curl and git. The built-in barq:// plugins are
# implemented as a single dispatcher script so that the image stays
# small and the plugin catalogue stays easy to audit.
#
# Custom plugins are built FROM this image so that they inherit the
# same runtime contract. The runtime contract is documented in
# docs/plugins.md.

FROM alpine:3.20

RUN apk add --no-cache \
      ca-certificates \
      curl \
      git \
      jq \
      tar \
    && addgroup -g 65532 -S barq \
    && adduser -u 65532 -S -G barq barq

COPY plugins/runtime/bin/ /usr/local/bin/

RUN chmod 0755 /usr/local/bin/barq-builtin \
    && mkdir -p /workspace \
    && chown 65532:65532 /workspace

USER 65532:65532

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/barq-builtin"]
