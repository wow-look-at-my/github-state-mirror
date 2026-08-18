# The binary is built by go-toolchain in CI and downloaded into build/ by the
# publish-ghcr reusable workflow before this image is built -- it is NOT compiled
# here. go-toolchain's build is a GOOS=cosmo fat APE (see cosmopatch/README.md):
# it writes exactly one file, server_cosmo_fat, and no per-platform name such as
# server_linux_amd64 (the go-toolchain CLI's own "matrix" subcommand also links
# convenience symlinks like server_host, but the plain build the CI Action runs
# does not -- only server_cosmo_fat is ever actually uploaded). The APE
# self-assimilates into a native ELF at exec time, so it runs on the distroless
# static base with no libc, same as a real static linux/amd64 binary would.

# Stage a writable, nonroot-owned data dir for the SQLite cache. distroless has
# no shell, so the directory (with correct ownership) is prepared in busybox and
# copied in.
FROM busybox:musl AS dirs
RUN mkdir -p /data && chown 65532:65532 /data

FROM gcr.io/distroless/static-debian12:nonroot

ARG VERSION=dev

LABEL org.opencontainers.image.source="https://github.com/wow-look-at-my/github-state-mirror"
LABEL org.opencontainers.image.version="${VERSION}"
LABEL org.opencontainers.image.description="Mirrors GitHub state into SQLite behind a fast local API"

COPY --chmod=755 build/server_cosmo_fat /usr/local/bin/github-state-mirror
COPY --from=dirs --chown=65532:65532 /data /var/lib/github-state-mirror

# The SQLite cache DB is disposable but needs a writable, nonroot-owned location.
ENV DB_PATH=/var/lib/github-state-mirror/github-mirror.db
ENV LISTEN_ADDR=:8080

EXPOSE 8080
VOLUME /var/lib/github-state-mirror

STOPSIGNAL SIGTERM

USER nonroot
ENTRYPOINT ["github-state-mirror"]
