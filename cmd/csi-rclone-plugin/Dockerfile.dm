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

COPY ./_output/csi-rclone-plugin-dm /bin/csi-rclone-plugin

ENTRYPOINT [ "/sbin/tini", "--"]
CMD ["/bin/csi-rclone-plugin"]
