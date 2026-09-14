# syntax=docker/dockerfile:1
#
# nuzur-agent — nuzur-cli, pinned and checksum-verified, for the in-cluster
# nuzur agent in front of metiche's database. docs/NUZUR_AGENT.md §6.1.
#
#   docker build -f deploy/docker/nuzur-agent.Dockerfile -t nuzur-agent:1.9.2-<git sha> deploy/docker
#
# The build context is deploy/docker, for the two helper scripts next to
# this file.
#
# NOTHING IS COMPILED HERE. The binary is nuzur's own GoReleaser release asset,
# a static CGO_ENABLED=0 build (nuzur-cli .goreleaser.yaml `builds.env`), so
# unlike the other two images this one is not architecture-sensitive to build.
# The box still builds it natively, like everything else
# (deploy/scripts/nuzur-agent-setup.sh image).
#
# ─────────────────────────────────────────────────────────────────────────────
# TWO CHECKSUMS, BOTH REQUIRED, BOTH BEFORE `tar`
#
#   1. The sha256 PINNED IN THIS FILE. That is the real pin: nuzur-cli's
#      releases use GoReleaser `release: mode: replace`, which allows assets
#      to be re-uploaded under the same tag, so a checksum downloaded next to
#      the asset only proves the two were uploaded together.
#   2. The line for the same asset in the release's own checksums file
#      (GoReleaser default name nuzur-cli_<version>_checksums.txt; the
#      .goreleaser.yaml has no `checksum:` block). If the release was
#      re-published, this disagrees with the pin and the build stops. Someone
#      then decides on purpose, never by accident.
#
# Asset names come from the GoReleaser archive name_template
# ({{ .ProjectName }}_{{ title .Os }}_{{ x86_64 for amd64 }}), and are the
# same names nuzur's own bootstrap uses:
#   linux/amd64 → nuzur-cli_Linux_x86_64.tar.gz   (production)
#   linux/arm64 → nuzur-cli_Linux_arm64.tar.gz    (local kind/docker tests on arm64)
#
# Upgrading (docs/NUZUR_AGENT.md §13 Q3: only when nuzur forces it): change
# NUZUR_CLI_VERSION and both sha256 values together, taken from a release you
# downloaded and checked yourself, then re-run P1 (§10.3).
# ─────────────────────────────────────────────────────────────────────────────

ARG RUNTIME_IMAGE=alpine:3.22.1

############################
# STEP 1 — fetch and verify
############################
FROM ${RUNTIME_IMAGE} AS fetch

ARG TARGETARCH
ARG NUZUR_CLI_VERSION=1.9.2
ARG NUZUR_CLI_SHA256_AMD64=ce16724527d0273092e15d771a521b0d11d1faf184ba05791dc13b07177261e0
ARG NUZUR_CLI_SHA256_ARM64=491dd762cf1a4c863b785559f12e8a990ab7ef63a547e9759585e1f163e6ea9e

RUN apk add --no-cache curl

WORKDIR /fetch
RUN set -eu; \
    case "${TARGETARCH:-amd64}" in \
        amd64) arch=x86_64; pin="${NUZUR_CLI_SHA256_AMD64}" ;; \
        arm64) arch=arm64;  pin="${NUZUR_CLI_SHA256_ARM64}" ;; \
        *) echo "nuzur-agent: no pinned nuzur-cli checksum for ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    base="https://github.com/nuzur/nuzur-cli/releases/download/v${NUZUR_CLI_VERSION}"; \
    asset="nuzur-cli_Linux_${arch}.tar.gz"; \
    sums="nuzur-cli_${NUZUR_CLI_VERSION}_checksums.txt"; \
    curl -fsSL --proto '=https' --tlsv1.2 -o "${asset}" "${base}/${asset}"; \
    curl -fsSL --proto '=https' --tlsv1.2 -o "${sums}" "${base}/${sums}"; \
    echo "nuzur-agent: verifying ${asset} against the sha256 pinned in the Dockerfile"; \
    printf '%s  %s\n' "${pin}" "${asset}" | sha256sum -c -; \
    echo "nuzur-agent: verifying the release checksums file agrees with the pin"; \
    grep -qx "${pin}  ${asset}" "${sums}" \
        || { echo "nuzur-agent: ${sums} does not list ${pin} for ${asset}; the release changed since it was pinned. Refusing." >&2; exit 1; }; \
    tar -xzf "${asset}" nuzur-cli; \
    chmod 0755 nuzur-cli

############################
# STEP 2 — runtime
############################
FROM ${RUNTIME_IMAGE}

ARG NUZUR_CLI_VERSION=1.9.2
LABEL org.opencontainers.image.title="nuzur-agent" \
      org.opencontainers.image.description="nuzur-cli ${NUZUR_CLI_VERSION} (checksum-pinned) as metiche's in-cluster nuzur agent" \
      org.opencontainers.image.source="https://github.com/mklfarha/metiche"

# ca-certificates: the CLI verifies TLS for cm.nuzur.com and product.nuzur.com
# against the system pool. Nothing else: no dbus and no dbus-launch, which is
# what guarantees the keyring falls through to the encrypted FILE backend on
# the PVC (docs/NUZUR_AGENT.md §4.D). Adding dbus to this image would silently
# move where the DSN is stored.
RUN apk add --no-cache ca-certificates \
 && addgroup -g 10001 nuzur \
 && adduser -D -H -u 10001 -G nuzur -h /var/lib/nuzur -s /sbin/nologin nuzur

COPY --from=fetch --chown=0:0 --chmod=0755 /fetch/nuzur-cli /usr/local/bin/nuzur-cli
COPY --chown=0:0 --chmod=0755 nuzur-agent-preflight nuzur-agent-hold /usr/local/bin/

# The same values the chart sets explicitly. Also here so that nothing run in
# this image, `docker run` included, can resolve the nuzur config dir to
# /tmp/nuzur-cli or derive a different keyring passphrase (§4.C, §4.D).
ENV HOME=/var/lib/nuzur \
    XDG_CONFIG_HOME=/var/lib/nuzur/config \
    USER=nuzur \
    LOGNAME=nuzur

USER 10001:10001
WORKDIR /var/lib/nuzur

ENTRYPOINT ["/usr/local/bin/nuzur-cli"]
CMD ["agent", "start"]
