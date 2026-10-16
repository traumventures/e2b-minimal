FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /e2b-minimal ./cmd/e2b-minimal

FROM alpine:3.22
RUN apk add --no-cache bash util-linux curl jq coreutils ca-certificates

# Released E2B artifacts this image runs, from e2b-dev/runtime embed/compose/.env
# (see UPSTREAM.md). fetch-artifacts.sh verifies each download's sha256.
ENV E2B_ORCHESTRATOR_VERSION=v0.16.202610051633-d1cd3db3890 \
    E2B_ENVD_VERSION=v0.9.202609130627-59497eb9134 \
    E2B_KERNEL_VERSION=vmlinux-6.1.177_5008931 \
    E2B_FIRECRACKER_VERSION=v1.14-0.2.0 \
    E2B_BUSYBOX_VERSION=1.36.1

COPY upstream/scripts/ /opt/e2b/scripts/
COPY entrypoint.sh /opt/e2b/entrypoint.sh
COPY --from=build /e2b-minimal /usr/local/bin/e2b-minimal

EXPOSE 3000
STOPSIGNAL SIGTERM
ENTRYPOINT ["/opt/e2b/entrypoint.sh"]
