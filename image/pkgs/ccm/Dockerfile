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
COPY --from=lib install-go.sh /usr/local/bin/
COPY ccm.yaml.in /ccm.yaml.in
WORKDIR /build
# naru-ccm, from the repository (the `src` context, which /.dockerignore narrows to the Go workspace).
COPY --from=src / /src
RUN (cd /src/node && go build -trimpath -buildvcs=false -ldflags='-s -w -buildid=' -o /build/naru-ccm ./cmd/naru-ccm) \
 && install-go.sh "$NARU_GO_VERSION" /build/naru-ccm /build/naru-ccm.checked
# The image: one uncompressed layer with /naru-ccm, in an OCI layout whose index names it for containerd
# (io.containerd.image.name), and the manifests that name it by digest.
RUN mkdir -p /oci/blobs/sha256 /rootfs /out/usr/lib/naru/addons/images /out/usr/lib/naru/addons/manifests \
 && install -m 0755 /build/naru-ccm.checked /rootfs/naru-ccm \
 && tar --sort=name --mtime="@$SOURCE_DATE_EPOCH" --owner=0 --group=0 --numeric-owner --format=gnu -cf /layer.tar -C /rootfs naru-ccm \
 && layer=$(sha256sum /layer.tar | cut -d' ' -f1) \
 && printf '{"architecture":"amd64","os":"linux","config":{"Entrypoint":["/naru-ccm"]},"rootfs":{"type":"layers","diff_ids":["sha256:%s"]}}' "$layer" >/config.json \
 && config=$(sha256sum /config.json | cut -d' ' -f1) \
 && printf '{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"sha256:%s","size":%s},"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar","digest":"sha256:%s","size":%s}]}' \
      "$config" "$(stat -c %s /config.json)" "$layer" "$(stat -c %s /layer.tar)" >/manifest.json \
 && digest=$(sha256sum /manifest.json | cut -d' ' -f1) \
 && cp /layer.tar /oci/blobs/sha256/$layer && cp /config.json /oci/blobs/sha256/$config && cp /manifest.json /oci/blobs/sha256/$digest \
 && printf '{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:%s","size":%s,"annotations":{"io.containerd.image.name":"naru.local/ccm:release"}}]}' \
      "$digest" "$(stat -c %s /manifest.json)" >/oci/index.json \
 && printf '{"imageLayoutVersion":"1.0.0"}' >/oci/oci-layout \
 && tar --sort=name --mtime="@$SOURCE_DATE_EPOCH" --owner=0 --group=0 --numeric-owner --format=gnu -cf /out/usr/lib/naru/addons/images/ccm.tar -C /oci . \
 && sed "s#@CCM_IMAGE@#naru.local/ccm@sha256:$digest#" /ccm.yaml.in >/out/usr/lib/naru/addons/manifests/cloud-controller.yaml \
 && ! grep -q '@[A-Z_]*@' /out/usr/lib/naru/addons/manifests/cloud-controller.yaml

FROM scratch
COPY --from=build /out /
