# Built on the *build* platform whatever the target, and cross-compiled to the
# target architecture by Go itself. Emulating an arm toolchain under QEMU to
# produce an arm image takes minutes; this takes seconds.
#
# Pinned to the Go that mise.toml names, rather than floating on a tag that
# would change the compiler under a rebuild with no commit to say so.
FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine AS build

WORKDIR /src

# The modules (godbus, gokit) are fetched in a layer of their own,
# which the source changing does not invalidate.
COPY go.mod go.sum ./
RUN go mod download

COPY main.go ./
COPY internal ./internal

# Computed by ./run from the git history, which is not in the build context -
# so it is passed in rather than worked out here. A build with no --build-arg
# still works and says "development", which is the truth about that build.
ARG VERSION=development

# TARGETVARIANT is "v7" for linux/arm/v7 and empty for the others; GOARM wants
# the bare number, and ignores it on every architecture but arm.
ARG TARGETARCH
ARG TARGETVARIANT
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH GOARM=${TARGETVARIANT#v} \
    go build -trimpath -ldflags "-s -w -X github.com/danielbodart/portical/internal/version.Version=$VERSION" \
    -o /out/usr/local/bin/portical .

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

# An ARG does not survive into the next stage, so it is declared again. These
# labels are how `docker inspect` answers what a pulled image actually is,
# which matters when every published tag also exists as :latest.
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
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/ /

# Portical only reads the Docker socket, so it does not need to be root - but
# it does need to be in the group that owns the socket. That group id varies by
# host, so this stays root by default. To drop it, give compose a
# `user: "65534:<the socket's gid>"` - by number, since an empty image has no
# /etc/passwd or /etc/group to name either in.
ENTRYPOINT ["/usr/local/bin/portical"]
CMD ["run"]
