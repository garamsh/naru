# The cloud-controller add-on (ADR 0085): naru-ccm, built from this repository's node module, an OCI
# archive of an image that holds only it, which every Node imports into containerd, and the manifests a
# control-plane Node applies when the cluster turns the add-on on, naming that image by its digest. The
# archive is laid out here, byte for byte from SOURCE_DATE_EPOCH, so the digest is the same for two builds
# of one commit. The binary is carried once, inside the archive.
FROM builder AS build
SHELL ["/bin/bash", "-euo", "pipefail", "-c"]
ARG SOURCE_DATE_EPOCH
ENV TZ=UTC LC_ALL=C SOURCE_DATE_EPOCH=${SOURCE_DATE_EPOCH} \
    GOPATH=/go GOCACHE=/gocache GOFLAGS=-mod=readonly \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64
COPY --from=lib install-go.sh addon-image.sh /usr/local/bin/
COPY ccm.yaml.in /ccm.yaml.in
WORKDIR /build
# naru-ccm, from the repository (the `src` context, which /.dockerignore narrows to the Go workspace).
COPY --from=src / /src
RUN (cd /src/node && go build -trimpath -buildvcs=false -ldflags='-s -w -buildid=' -o /build/naru-ccm ./cmd/naru-ccm) \
 && install-go.sh "$NARU_GO_VERSION" /build/naru-ccm /build/naru-ccm.checked
# The image: one uncompressed layer with /naru-ccm, in an OCI layout whose index names it for containerd
# (io.containerd.image.name) by the reference the manifests use. addon-image.sh makes the archive and
# prints that reference, which is written into cloud-controller.yaml and checked against the archive.
RUN mkdir -p /rootfs /out/usr/lib/naru/addons/images /out/usr/lib/naru/addons/manifests \
 && install -m 0755 /build/naru-ccm.checked /rootfs/naru-ccm \
 && ref=$(addon-image.sh build ccm naru-ccm /rootfs /out/usr/lib/naru/addons/images/ccm.tar) \
 && sed "s#@CCM_IMAGE@#$ref#" /ccm.yaml.in >/out/usr/lib/naru/addons/manifests/cloud-controller.yaml \
 && ! grep -q '@[A-Z_]*@' /out/usr/lib/naru/addons/manifests/cloud-controller.yaml \
 && addon-image.sh check ccm /out/usr/lib/naru/addons/images/ccm.tar /out/usr/lib/naru/addons/manifests/cloud-controller.yaml

FROM scratch
COPY --from=build /out /
