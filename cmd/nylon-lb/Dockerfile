# Release image for nylon-lb, built by GoReleaser (dockers_v2).
# The binary is already compiled per platform, so the build context contains
# <os>/<arch>/nylon-lb plus the license files listed in extra_files — the source
# tree is NOT part of the context.
# The image intentionally has no USER directive: the speaker binds allocated
# /32s on lo (requires NET_ADMIN) and writes announce files into a root-owned
# hostPath; deploy/daemonset.yaml sets runAsUser: 0 explicitly.
# Local dev image (compiles from source, with a debug stage): Dockerfile.dev
FROM scratch

# TARGETPLATFORM is a buildx predefined ARG. It must be re-declared inside the
# stage: a global ARG (before FROM) is not in scope for instructions after FROM,
# and without this the COPY below expands to an empty platform path.
ARG TARGETPLATFORM

COPY $TARGETPLATFORM/nylon-lb /usr/local/bin/nylon-lb
COPY NOTICE LICENSE LICENSE.GPL-3.0 /usr/share/doc/nylon/
COPY licenses /usr/share/doc/nylon/licenses/

EXPOSE 9633
ENTRYPOINT ["/usr/local/bin/nylon-lb"]
