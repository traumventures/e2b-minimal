# The orchestrator, built from the public e2b-dev/runtime source plus one
# small patch (orchestrator/memory.patch). It runs on the host, so it links
# against bookworm's glibc like E2B's own build.
FROM golang:1.26.8-bookworm AS orchestrator
ARG RUNTIME_COMMIT=e456ecfcf4bc878616fba7c89bc0ba946acf6afd
RUN git init -q /src && cd /src \
 && git remote add origin https://github.com/e2b-dev/runtime.git \
 && git fetch -q --depth 1 origin "$RUNTIME_COMMIT" \
 && git checkout -q FETCH_HEAD
COPY orchestrator/memory.patch /tmp/memory.patch
RUN cd /src && git apply /tmp/memory.patch
WORKDIR /src/packages/orchestrator
RUN --mount=type=cache,target=/root/.cache/go-build --mount=type=cache,target=/go/pkg/mod \
    make build-local COMMIT_SHA="$RUNTIME_COMMIT" VERSION=e2b-minimal BUILD_ARCH=amd64

FROM golang:1.26-alpine AS api
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /e2b-minimal ./cmd/e2b-minimal

FROM alpine:3.22
RUN apk add --no-cache bash util-linux curl jq coreutils ca-certificates

# Released E2B artifacts, from e2b-dev/runtime embed/compose/.env (see
# UPSTREAM.md). fetch-artifacts.sh verifies each download's sha256; the
# orchestrator it fetches is replaced by the one built above.
ENV E2B_ORCHESTRATOR_VERSION=v0.16.202610051633-d1cd3db3890 \
    E2B_ENVD_VERSION=v0.9.202609130627-59497eb9134 \
    E2B_KERNEL_VERSION=vmlinux-6.1.177_5008931 \
    E2B_FIRECRACKER_VERSION=v1.14-0.2.0 \
    E2B_BUSYBOX_VERSION=1.36.1

COPY upstream/scripts/ /opt/e2b/scripts/
COPY entrypoint.sh /opt/e2b/entrypoint.sh
COPY --from=orchestrator /src/packages/orchestrator/bin/orchestrator /opt/e2b/bin/orchestrator
COPY --from=api /e2b-minimal /usr/local/bin/e2b-minimal

EXPOSE 3000
STOPSIGNAL SIGTERM
ENTRYPOINT ["/opt/e2b/entrypoint.sh"]
