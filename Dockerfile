# The binary `mise run dist` built for the target architecture, and nothing
# compiled here: the release binaries and every image come from the one compile,
# by the Go mise.toml pins, so they cannot disagree.
#
# Only what an empty image cannot make for itself comes from Alpine, pinned by
# digest so a rebuild cannot change it with no commit to say so; the weekly
# update moves the digest. Its stage runs on the build platform whatever the
# target, since nothing taken from it depends on the architecture.
FROM --platform=$BUILDPLATFORM alpine:3.24@sha256:be2656061b536c1883c4034790e21d0a09c5aaaf51968b89368a3c0371ea2c9a AS base

# v1 was a shell script at this path, and its README told people to write
# `command: "/opt/portical/run poll"`. Those compose files are still running.
# The argument parser already ignores a leading path to the old script, and
# this covers anyone who also overrode the entrypoint to point straight at it.
# Made here because the image below has no shell to make it in.
RUN mkdir -p /out/opt/portical && ln -s /usr/local/bin/portical /out/opt/portical/run

# Nothing but the binary. A static Go binary needs no libc, and v1's Docker CLI
# and miniupnpc are long gone - Portical speaks the Docker Engine API and UPnP
# SOAP itself. The macvlan relay runs this same image as a helper, but only as
# `portical relay <request>` through the entrypoint, so it needs no shell
# either. And no tini: Portical handles SIGTERM itself, and starts no children
# to reap.
FROM scratch

# Computed by ./run from the git history and passed in, as dist/ was built
# with it. These labels are how `docker inspect` answers what a pulled image
# actually is, which matters when every published tag also exists as :latest.
ARG VERSION=development
LABEL org.opencontainers.image.version="$VERSION" \
      org.opencontainers.image.title="portical" \
      org.opencontainers.image.description="Manage UPnP port forwarding rules for Docker containers and systemd units" \
      org.opencontainers.image.source="https://github.com/danielbodart/portical" \
      org.opencontainers.image.licenses="Apache-2.0"

# For a gateway whose --root is https. UPnP is plain HTTP, and so is every
# router seen so far, but the option accepts https, and a bundle of a few
# hundred kilobytes is cheaper than an x509 error nobody can fix from outside
# the image.
COPY --from=base /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=base /out/ /

# TARGETVARIANT is "v7" for linux/arm/v7 and empty for the others, so this is
# dist/linux-amd64, dist/linux-arm64 or dist/linux-armv7.
ARG TARGETARCH
ARG TARGETVARIANT
COPY dist/linux-${TARGETARCH}${TARGETVARIANT}/portical /usr/local/bin/portical

# Portical only reads the Docker socket, so it does not need to be root - but
# it does need to be in the group that owns the socket. That group id varies by
# host, so this stays root by default. To drop it, give compose a
# `user: "65534:<the socket's gid>"` - by number, since an empty image has no
# /etc/passwd or /etc/group to name either in.
ENTRYPOINT ["/usr/local/bin/portical"]
CMD ["run"]
