# Empty state directory for the final image. distroless has no shell, so the
# directory is created here (WORKDIR needs no shell or image pull) and copied
# with nonroot ownership below. Uses nothing from the build context, so the
# GoReleaser context (binaries only) keeps working.
FROM scratch AS state-dir
WORKDIR /var/lib/bb-daemon

FROM gcr.io/distroless/static:nonroot
COPY bb-daemon /usr/local/bin/bb-daemon
COPY bb-browser /usr/local/bin/bb-browser
# A fresh named volume mounted here inherits this ownership (uid/gid 65532).
COPY --from=state-dir --chown=65532:65532 /var/lib/bb-daemon /var/lib/bb-daemon
USER nonroot:nonroot
EXPOSE 8787
ENV BB_BROWSER_LISTEN=0.0.0.0:8787
ENTRYPOINT ["/usr/local/bin/bb-daemon"]
