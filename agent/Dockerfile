# The backup sidecar: accepts an archive from the service next to it and forwards it to
# backio, adding the token, the naming, optional encryption and the retention policy.
#
# Built from the repo root, so it can see go.mod and internal/:
#   docker build -f agent/Dockerfile -t ghcr.io/reeywhaar/backio-agent .
#
# A separate image rather than a second command in the main one. It runs beside every
# project rather than once per host, it holds a token instead of issuing them, and
# keeping it out means the server image gains neither 7-Zip nor a reason to carry it.
FROM --platform=$BUILDPLATFORM golang:alpine AS builder
ARG TARGETOS TARGETARCH
WORKDIR /src
# No `go mod download`: the module has no dependencies, so there is nothing to fetch.
COPY go.mod ./
COPY internal/ internal/
COPY agent/ agent/
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
      -ldflags="-s -w" -o /out/agent ./agent

# 7-Zip's own static build rather than `apk add 7zip`, which is 4.8 MB: the packaged 7zz
# links libstdc++, and that shared library alone is 2.8 MB of an image whose Go binary is
# a few. This is the same program producing the same AES-256 zip — nothing about
# restoring changes.
#
# Pinned by checksum, which is the part that earns the swap. An apk package arrives
# signed; a tarball from a web server does not, so the digest is the only thing standing
# between a compromised mirror and a binary that handles the backup password. Bump both
# together.
FROM --platform=$BUILDPLATFORM alpine AS sevenzip
ARG TARGETARCH
ARG SEVENZIP=7z2501
RUN apk add --no-cache curl tar xz
RUN case "$TARGETARCH" in \
      amd64) arch=x64;   sha=4ca3b7c6f2f67866b92622818b58233dc70367be2f36b498eb0bdeaaa44b53f4 ;; \
      arm64) arch=arm64; sha=39c5140f02ce4436599303c59a149f654cb1bbc47cdc105a942120d747ae040d ;; \
      *) echo "no 7-Zip build pinned for $TARGETARCH" >&2; exit 1 ;; \
    esac && \
    curl -fsSL -o /tmp/7z.tar.xz "https://www.7-zip.org/a/${SEVENZIP}-linux-${arch}.tar.xz" && \
    echo "${sha}  /tmp/7z.tar.xz" | sha256sum -c - && \
    xz -dc /tmp/7z.tar.xz | tar -x -C /tmp 7zzs && \
    install -D -m 0755 /tmp/7zzs /out/7zzs

FROM alpine:latest
# ca-certificates because BACKIO_HOST may be an https address on another network.
RUN apk add --no-cache ca-certificates

# 7zzs is the static one; the 7zz in the same tarball is glibc-linked and will not run
# here. Symlinked so the plain "7z" the agent runs resolves.
COPY --from=sevenzip /out/7zzs /usr/local/bin/7zz
RUN ln -s 7zz /usr/local/bin/7z

COPY --from=builder /out/agent /agent

ENV PORT=8080
EXPOSE 8080

# The check is the agent's own /health, called from inside the container, so it needs no
# tool the image would not otherwise ship. Without BACKUP_EXPECT_EVERY that only proves
# the agent is serving; with it, the container goes unhealthy when archives stop
# arriving — which turns "backups stopped working in March" into something `docker ps`
# says in March.
HEALTHCHECK --interval=1m --timeout=10s --start-period=10s --retries=3 \
  CMD ["/agent", "healthcheck"]

# No `VOLUME /backups`, and the directory is deliberately not created here either. Docker
# creates a mount target the image is missing, so an existing /backups means a volume is
# mounted and the sidecar keeps local copies as well as forwarding; no directory means it
# forwards and keeps none. `VOLUME` would break that by conjuring an anonymous volume
# nobody named and nothing prunes.
CMD ["/agent"]
