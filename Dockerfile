# The binaries are cross-compiled by Go before this runs, so nothing executes
# during the build and a foreign platform needs no emulation. That is why the
# multi-platform image needs no QEMU. Adding a RUN would change that.
#
# The build context is not the repo: goreleaser stages a temporary directory
# holding one binary per platform under <goos>/<goarch>, which is exactly what
# buildx puts in TARGETPLATFORM. Building this file with plain docker build
# from the repo root will not find the binary.
FROM gcr.io/distroless/static:nonroot

ARG TARGETPLATFORM
COPY ${TARGETPLATFORM}/fasync /usr/local/bin/fasync

# distroless/static carries the CA certificates the API calls need and the
# timezone database the date arithmetic wants, neither of which scratch has.

# Two mounts, because they have different lifetimes and different sensitivity.
# /data is the archive and can be backed up freely; /config holds the OAuth
# refresh token, which rotates on use, so this mount has to be writable or the
# next run authenticates from scratch.
ENV XDG_DATA_HOME=/data
ENV XDG_CONFIG_HOME=/config
VOLUME ["/data", "/config"]

# nonroot is uid 65532 and is already this image's user, so both mounts have to
# be writable by it.
WORKDIR /data

ENTRYPOINT ["/usr/local/bin/fasync"]
CMD ["status"]
