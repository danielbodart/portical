# Built on the *build* platform whatever the target, and cross-compiled to the
# target architecture by Bun itself. Emulating an arm64 toolchain under QEMU to
# produce an arm64 image takes minutes; this takes seconds.

# The Bun that compiles the binary, and so the runtime that ends up inside it.
# Kept in step with mise.toml - the one place the version is written down - by
# `bun run.ts check`, which fails if they disagree; `bun run.ts sync` rewrites
# this line from it. Carried here as well as there so that `docker build .` on a
# clean checkout is still correct without mise installed.
#
# Pinned rather than floating on :1, which silently took the published image
# from 1.3.14 to 1.4.0 on the first rebuild after that release - no commit, no
# changelog, and a runtime nobody had tested against.
ARG BUN_VERSION=1.4.2
FROM --platform=$BUILDPLATFORM oven/bun:${BUN_VERSION}-alpine AS build

WORKDIR /app

COPY package.json bun.lock ./
RUN bun install --frozen-lockfile

COPY tsconfig.json ./
COPY src ./src

# Computed by run.ts from the git history, which is not in the build context -
# so it is passed in rather than worked out here. A build with no --build-arg
# still works and says "development", which is the truth about that build.
ARG VERSION=development

ARG TARGETARCH
RUN target="bun-linux-$([ "$TARGETARCH" = "arm64" ] && echo arm64 || echo x64)-musl" && \
    bun build --compile --minify --define "PORTICAL_VERSION=\"$VERSION\"" --target="$target" src/main.ts --outfile portical

FROM alpine:3

# An ARG does not survive into the next stage, so it is declared again. These
# labels are how `docker inspect` answers what a pulled image actually is,
# which matters when every published tag also exists as :latest.
ARG VERSION=development
LABEL org.opencontainers.image.version="$VERSION" \
      org.opencontainers.image.title="portical" \
      org.opencontainers.image.description="Manage UPnP port forwarding rules for Docker containers with a single label" \
      org.opencontainers.image.source="https://github.com/danielbodart/portical" \
      org.opencontainers.image.licenses="Apache-2.0"

# Bun's compiled output links against the C++ runtime. Nothing else is needed:
# v1's image carried the Docker CLI and miniupnpc, and both are now gone -
# Portical speaks the Docker Engine API and UPnP SOAP itself.
RUN apk add --no-cache libstdc++ ca-certificates

COPY --from=build /app/portical /usr/local/bin/portical

# v1 was a shell script at this path, and its README told people to write
# `command: "/opt/portical/run poll"`. Those compose files are still running.
# The argument parser already ignores a leading path to the old script, and
# this covers anyone who also overrode the entrypoint to point straight at it.
RUN mkdir -p /opt/portical && ln -s /usr/local/bin/portical /opt/portical/run

# Portical only reads the Docker socket, so it does not need to be root - but
# it does need to be in the group that owns the socket. That group id varies by
# host, so this stays root by default and the compose file shows the override.
ENTRYPOINT ["/usr/local/bin/portical"]
CMD ["run"]
