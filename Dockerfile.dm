####
# Pinned Go toolchain. go.mod declares `go 1.15` with ancient k8s 1.13.2 libs;
# this tag is known to compile the module (see CHANGELOG 1.6.0).
FROM golang:1.24-alpine AS builder
RUN apk update && apk add --no-cache git make bash
WORKDIR $GOPATH/src/csi-rclone-nodeplugin
COPY . .
RUN make plugin-dm

####
FROM alpine:3.19
RUN apk add --no-cache ca-certificates bash fuse3 curl unzip tini

# Offline (dm) build: rclone is installed from the pre-downloaded zips under
# rclone-build/ via install-dm.sh, NOT from the network. The expected pinned
# rclone version for rclone-build/ is v1.74.3 (keep in sync with the online
# Dockerfiles). Populate rclone-build/ with rclone-current-linux-<arch>.zip
# from https://downloads.rclone.org/v1.74.3/ before building.
# Use pre-compiled version (with cirectory marker patch)
# https://github.com/rclone/rclone/pull/5323
COPY ./install-dm.sh /tmp
COPY ./rclone-build /tmp/rclone-build
RUN /tmp/install-dm.sh

COPY --from=builder /go/src/csi-rclone-nodeplugin/_output/csi-rclone-plugin-dm /bin/csi-rclone-plugin

ENTRYPOINT [ "/sbin/tini", "--"]
CMD ["/bin/csi-rclone-plugin"]