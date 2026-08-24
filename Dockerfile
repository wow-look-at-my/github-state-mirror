# The binary is built by go-toolchain in CI and downloaded into build/ by the
# publish-ghcr reusable workflow before this image is built -- it is NOT compiled
# here. go-toolchain's build is a GOOS=cosmo fat APE: it writes exactly one
# file, named the bare binary name (server), and no per-platform name such as
# server_linux_amd64 or server_cosmo_fat.
#
# An APE needs a SHELL to start. The kernel refuses the file -- it begins with
# the DOS/shell magic, not an ELF header -- so a shell runs its boot script,
# which writes the real header over a copy it stages under /tmp.
# see https://github.com/wow-look-at-my/gosmopolitan/blob/master/docs/APE-STAGING.md
#
# distroless ships no shell, so busybox supplies one. The base stays distroless
# because it carries the CA certificates every upstream GitHub call needs.
# busybox also prepares the two directories, since distroless has no shell to
# run mkdir in: the SQLite cache dir, and /tmp, which staging writes to.
FROM busybox:musl AS shell
# The applet links are made by hand, RELATIVE. `busybox --install -s` writes
# each one as an absolute path to where it ran, so every link would dangle once
# this directory is copied to /bin below.
RUN mkdir -p /out/bin /out/data /out/tmp \
 && cp /bin/busybox /out/bin/busybox \
 && cd /out/bin \
 && for a in $(./busybox --list); do [ "$a" = busybox ] || ln -sf busybox "$a"; done \
 && chown 65532:65532 /out/data /out/tmp \
 && touch /out/tmp/.keep

FROM gcr.io/distroless/static-debian12:nonroot

ARG VERSION=dev

LABEL org.opencontainers.image.source="https://github.com/wow-look-at-my/github-state-mirror"
LABEL org.opencontainers.image.version="${VERSION}"
LABEL org.opencontainers.image.description="Mirrors GitHub state into SQLite behind a fast local API"

COPY --from=shell /out/bin/ /bin/
COPY --chmod=755 build/server /usr/local/bin/github-state-mirror
COPY --from=shell --chown=65532:65532 /out/data /var/lib/github-state-mirror
COPY --from=shell --chown=65532:65532 /out/tmp /tmp

# The SQLite cache DB is disposable but needs a writable, nonroot-owned location.
ENV DB_PATH=/var/lib/github-state-mirror/github-mirror.db
ENV LISTEN_ADDR=:8080

EXPOSE 8080
VOLUME /var/lib/github-state-mirror

STOPSIGNAL SIGTERM

USER nonroot

# Through the shell ON PURPOSE. The exec form execve()s the file directly, and
# the kernel cannot exec an APE -- that ENOEXEC is what left this image unable
# to start. The boot script execs the staged copy, so the server still replaces
# the shell as PID 1 and STOPSIGNAL reaches it.
ENTRYPOINT ["/bin/sh", "/usr/local/bin/github-state-mirror"]
