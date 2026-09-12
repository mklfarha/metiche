# syntax=docker/dockerfile:1
#
# The board (code/frontend). Build context is code/frontend:
#
#   docker build -f deploy/docker/metiche-web.Dockerfile -t metiche-web:TAG code/frontend
#
# It lives here rather than in code/frontend because deploy/ owns how this is
# packaged; the frontend itself is a plain `go build` and knows nothing about
# containers.
#
# BUILD THIS ON THE TARGET BOX. There is no --platform here on purpose: adding
# one silently turns a native build into a qemu-emulated one that takes twenty
# minutes and produces an image for the wrong architecture if you get the
# direction wrong. deploy/scripts/build-images.sh refuses to run on an
# architecture that does not match the target instead.

ARG GO_IMAGE=golang:1.26-alpine
ARG RUNTIME_IMAGE=alpine:3.22.1

############################
# STEP 1 — build
############################
FROM ${GO_IMAGE} AS builder

WORKDIR /src

# Dependencies first, keyed only on the module files, so a code change does
# not re-download the module graph.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .

# CGO off: the board opens no database and links nothing native, and a static
# binary means the runtime image needs no libc compatibility story.
#
# `go build ./...` first as a compile check over every package — the templ
# output and the view components are only reachable from the server, and a
# build of just ./cmd would still succeed with a broken package nothing in the
# main path imports yet.
ENV CGO_ENABLED=0
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build ./... && go build -trimpath -ldflags="-s -w" -o /out/metiche-web ./cmd/metiche-web

############################
# STEP 2 — runtime
############################
FROM ${RUNTIME_IMAGE}

# ca-certificates only if the board is ever pointed at an https backend
# through the public ingress. Nothing else: no shell tooling it does not need.
RUN apk --no-cache add ca-certificates

COPY --from=builder /out/metiche-web /usr/local/bin/metiche-web

# Static assets and the fixture recordings are compiled into the binary with
# go:embed (code/frontend/embed.go), so there is nothing else to copy and
# nothing to keep in sync at deploy time.

# Unlike the generated backend image, the binary is not in /root, so this can
# run as a normal user — and the chart sets runAsNonRoot to hold it to that.
USER 65532:65532

EXPOSE 8787
ENTRYPOINT ["/usr/local/bin/metiche-web"]
