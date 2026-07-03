####
# Pinned Go toolchain. go.mod declares `go 1.15` with ancient k8s 1.13.2 libs;
# this tag is known to compile the module (see CHANGELOG 1.6.0).
FROM golang:1.24-alpine AS builder
RUN apk update && apk add --no-cache git make bash
WORKDIR $GOPATH/src/csi-rclone-nodeplugin
COPY . .
RUN make plugin

####
FROM alpine:3.19
RUN apk add --no-cache ca-certificates bash fuse3 curl unzip tini

# Pin rclone to a specific release. rclone's install.sh only supports "latest
# stable" or "beta" (no version pin), so we download the pinned, per-arch
# release directly and verify its SHA256. TARGETARCH is populated by BuildKit
# (both `docker build` and `docker buildx build --platform ...`).
ARG TARGETARCH
ARG RCLONE_VERSION=v1.74.3
RUN set -eux; \
    arch="${TARGETARCH:-}"; \
    if [ -z "$arch" ]; then \
        case "$(uname -m)" in \
            x86_64) arch=amd64 ;; \
            aarch64) arch=arm64 ;; \
            *) echo "unsupported host arch: $(uname -m)" >&2; exit 1 ;; \
        esac; \
    fi; \
    case "$arch" in \
        amd64) rclone_sha256="dbee7ccd7a5d617e4ed4cd4555c16669b511abfe8d31164f61be35ac9e999bd2" ;; \
        arm64) rclone_sha256="8f8d47446e061f80c3256659fe8e21f56d72d96aaefe1275d088ea5eb6b42aa7" ;; \
        *) echo "unsupported TARGETARCH: $arch" >&2; exit 1 ;; \
    esac; \
    curl -fsSL -o /tmp/rclone.zip "https://downloads.rclone.org/${RCLONE_VERSION}/rclone-${RCLONE_VERSION}-linux-${arch}.zip"; \
    echo "${rclone_sha256}  /tmp/rclone.zip" | sha256sum -c -; \
    unzip -j /tmp/rclone.zip "rclone-${RCLONE_VERSION}-linux-${arch}/rclone" -d /usr/bin; \
    chmod 755 /usr/bin/rclone; \
    chown root:root /usr/bin/rclone; \
    rm -f /tmp/rclone.zip

COPY --from=builder /go/src/csi-rclone-nodeplugin/_output/csi-rclone-plugin /bin/csi-rclone-plugin

ENTRYPOINT [ "/sbin/tini", "--"]
CMD ["/bin/csi-rclone-plugin"]
