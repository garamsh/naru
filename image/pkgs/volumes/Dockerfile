# The volume add-on (ADR 0083, 0084): naru-volumes, the CSI driver of the repository's node module, and
# the external-provisioner and node-driver-registrar from the pinned commits, all static, in one
# image whose containers differ by command; an OCI archive of it, laid out here byte for byte from
# SOURCE_DATE_EPOCH so the digest is the same for two builds of one commit; the manifests that name it
# by digest; and the system extension naru-volumes, a squashfs that carries both. The volume driver
# does not fit the initramfs (ADR 0083: over 256 MiB), so it is an extension that init mounts and the
# Node's services read (lib/layout addonDir). NARU_VERSION is the version the extension is built for:
# init resolves the unpinned entry of spec.extensions to the image whose manifest names it.
FROM builder AS build
SHELL ["/bin/bash", "-euo", "pipefail", "-c"]
ARG SOURCE_DATE_EPOCH
ARG NARU_VERSION
ENV TZ=UTC LC_ALL=C SOURCE_DATE_EPOCH=${SOURCE_DATE_EPOCH} \
    GOPATH=/go GOCACHE=/gocache GOFLAGS=-mod=readonly \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64
RUN : "${NARU_VERSION:?volumes: NARU_VERSION is the version the extension is built for}"
COPY --from=lib gitfetch.sh install-go.sh /usr/local/bin/
COPY pin.env /pin.env
COPY volumes.yaml.in /volumes.yaml.in
WORKDIR /build
# naru-volumes, from the repository (the `src` context, which /.dockerignore narrows to the Go workspace).
COPY --from=src / /src
RUN (cd /src/node && go build -trimpath -buildvcs=false -ldflags='-s -w -buildid=' -o /build/naru-volumes ./cmd/naru-volumes) \
 && install-go.sh "$NARU_GO_VERSION" /build/naru-volumes /build/naru-volumes.checked
# The sidecars: each is built with the Go its go.mod asks for, and checked to be that Go's static program.
RUN . /pin.env && gitfetch.sh "$PROVISIONER_REPO" "$PROVISIONER_COMMIT" provisioner \
 && cd provisioner \
 && export GOTOOLCHAIN=go$(awk '$1 == "go" { print $2 }' go.mod) GOWORK=off && [ "$GOTOOLCHAIN" != go ] \
 && go build -trimpath -buildvcs=false -ldflags="-s -w -buildid= -X main.version=$PROVISIONER_VERSION" -o /build/csi-provisioner ./cmd/csi-provisioner \
 && install-go.sh "$GOTOOLCHAIN" /build/csi-provisioner /build/csi-provisioner.checked
RUN . /pin.env && gitfetch.sh "$REGISTRAR_REPO" "$REGISTRAR_COMMIT" registrar \
 && cd registrar \
 && export GOTOOLCHAIN=go$(awk '$1 == "go" { print $2 }' go.mod) GOWORK=off && [ "$GOTOOLCHAIN" != go ] \
 && go build -trimpath -buildvcs=false -ldflags="-s -w -buildid= -X main.version=$REGISTRAR_VERSION" -o /build/csi-node-driver-registrar ./cmd/csi-node-driver-registrar \
 && install-go.sh "$GOTOOLCHAIN" /build/csi-node-driver-registrar /build/csi-node-driver-registrar.checked
# The image: one uncompressed layer with the three programs, in an OCI layout whose index names it for
# containerd (io.containerd.image.name), and the manifests that name it by digest.
RUN mkdir -p /oci/blobs/sha256 /rootfs /tree/addons/images /tree/addons/manifests \
 && for p in naru-volumes csi-provisioner csi-node-driver-registrar; do install -m 0755 "/build/$p.checked" "/rootfs/$p"; done \
 && tar --sort=name --mtime="@$SOURCE_DATE_EPOCH" --owner=0 --group=0 --numeric-owner --format=gnu -cf /layer.tar -C /rootfs naru-volumes csi-provisioner csi-node-driver-registrar \
 && layer=$(sha256sum /layer.tar | cut -d' ' -f1) \
 && printf '{"architecture":"amd64","os":"linux","config":{"Entrypoint":["/naru-volumes"]},"rootfs":{"type":"layers","diff_ids":["sha256:%s"]}}' "$layer" >/config.json \
 && config=$(sha256sum /config.json | cut -d' ' -f1) \
 && printf '{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"sha256:%s","size":%s},"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar","digest":"sha256:%s","size":%s}]}' \
      "$config" "$(stat -c %s /config.json)" "$layer" "$(stat -c %s /layer.tar)" >/manifest.json \
 && digest=$(sha256sum /manifest.json | cut -d' ' -f1) \
 && cp /layer.tar /oci/blobs/sha256/$layer && cp /config.json /oci/blobs/sha256/$config && cp /manifest.json /oci/blobs/sha256/$digest \
 && printf '{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:%s","size":%s,"annotations":{"io.containerd.image.name":"naru.local/volumes:release"}}]}' \
      "$digest" "$(stat -c %s /manifest.json)" >/oci/index.json \
 && printf '{"imageLayoutVersion":"1.0.0"}' >/oci/oci-layout \
 && tar --sort=name --mtime="@$SOURCE_DATE_EPOCH" --owner=0 --group=0 --numeric-owner --format=gnu -cf /tree/addons/images/volumes.tar -C /oci . \
 && sed "s#@VOLUMES_IMAGE@#naru.local/volumes@sha256:$digest#" /volumes.yaml.in >/tree/addons/manifests/volumes.yaml \
 && ! grep -q '@[A-Z_]*@' /tree/addons/manifests/volumes.yaml
# The extension: the same squashfs as the extension package makes (zstd, one processor, root ownership,
# times fixed by SOURCE_DATE_EPOCH), with no service: the add-on runs in Pods.
RUN name=naru-volumes tree=/tree \
 && find "$tree" -type d -exec chmod 0755 {} + && find "$tree" -type f -exec chmod 0644 {} + \
 && printf '{"name":"%s","version":"%s","services":[]}\n' "$name" "$NARU_VERSION" >"$tree/manifest.json" \
 && mkdir -p /out/extensions \
 && mksquashfs "$tree" "/out/extensions/$name.sqfs" -comp zstd -reproducible -all-root -no-xattrs -noappend -processors 1 -quiet >/dev/null \
 && sha=$(sha256sum "/out/extensions/$name.sqfs" | cut -d' ' -f1) \
 && printf '{"name":"%s","version":"%s","sha256":"%s","services":[]}\n' "$name" "$NARU_VERSION" "$sha" >"/out/extensions/$name.json"

FROM scratch
COPY --from=build /out /
