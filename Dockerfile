# Built on the *build* platform whatever the target, and cross-compiled to the
# target architecture by Bun itself. Emulating an arm64 toolchain under QEMU to
# produce an arm64 image takes minutes; this takes seconds.
FROM --platform=$BUILDPLATFORM oven/bun:1-alpine AS build

WORKDIR /app

COPY package.json bun.lock ./
RUN bun install --frozen-lockfile

COPY tsconfig.json ./
COPY src ./src

ARG TARGETARCH
RUN target="bun-linux-$([ "$TARGETARCH" = "arm64" ] && echo arm64 || echo x64)-musl" && \
    bun build --compile --minify --target="$target" src/main.ts --outfile portical

FROM alpine:3

# Bun's compiled output links against the C++ runtime. Nothing else is needed:
# v1's image carried the Docker CLI and miniupnpc, and both are now gone -
# Portical speaks the Docker Engine API and UPnP SOAP itself.
RUN apk add --no-cache libstdc++ ca-certificates

COPY --from=build /app/portical /usr/local/bin/portical

# Portical only reads the Docker socket, so it does not need to be root - but
# it does need to be in the group that owns the socket. That group id varies by
# host, so this stays root by default and the compose file shows the override.
ENTRYPOINT ["/usr/local/bin/portical"]
CMD ["run"]
