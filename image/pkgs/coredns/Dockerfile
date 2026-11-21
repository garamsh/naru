# CoreDNS and the add-on that runs it (ADR 0083), from the pinned commit: a static coredns, an OCI
# archive of an image that holds only it, which every Node imports into containerd, and the manifests
# a control-plane Node applies, naming that image by its digest. The archive is laid out here, byte
# for byte from SOURCE_DATE_EPOCH, so the digest is the same for two builds of one commit.
FROM builder AS build
SHELL ["/bin/bash", "-euo", "pipefail", "-c"]
ARG SOURCE_DATE_EPOCH
ENV TZ=UTC LC_ALL=C SOURCE_DATE_EPOCH=${SOURCE_DATE_EPOCH} \
    GOPATH=/go GOCACHE=/gocache GOFLAGS=-mod=readonly GOWORK=off \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64
COPY --from=lib gitfetch.sh install-go.sh /usr/local/bin/
COPY pin.env /pin.env
COPY dns.yaml.in /dns.yaml.in
WORKDIR /build
RUN . /pin.env && gitfetch.sh "$COREDNS_REPO" "$COREDNS_COMMIT" src
RUN . /pin.env && cd src \
 && export GOTOOLCHAIN=go$(awk '$1 == "go" { print $2 }' go.mod) && [ "$GOTOOLCHAIN" != go ] \
 && go build -trimpath -buildvcs=false \
      -ldflags="-s -w -buildid= -X github.com/coredns/coredns/coremain.GitCommit=$COREDNS_COMMIT" \
      -o /build/coredns . \
 && install-go.sh "$GOTOOLCHAIN" /build/coredns /build/coredns.checked \
 && [ "$(/build/coredns -version | awk 'NR == 1 { print $1 }')" = "CoreDNS-${COREDNS_VERSION#v}" ]
# The image: one uncompressed layer with /coredns, a configuration and a manifest, in an OCI layout
# whose index names it for containerd (io.containerd.image.name), and the manifests that name it by digest.
RUN . /pin.env \
 && mkdir -p /oci/blobs/sha256 /rootfs /out/usr/lib/naru/addons/images /out/usr/lib/naru/addons/manifests \
 && install -m 0755 /build/coredns /rootfs/coredns \
 && tar --sort=name --mtime="@$SOURCE_DATE_EPOCH" --owner=0 --group=0 --numeric-owner --format=gnu -cf /layer.tar -C /rootfs coredns \
 && layer=$(sha256sum /layer.tar | cut -d' ' -f1) \
 && printf '{"architecture":"amd64","os":"linux","config":{"Entrypoint":["/coredns"]},"rootfs":{"type":"layers","diff_ids":["sha256:%s"]}}' "$layer" >/config.json \
 && config=$(sha256sum /config.json | cut -d' ' -f1) \
 && printf '{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"sha256:%s","size":%s},"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar","digest":"sha256:%s","size":%s}]}' \
      "$config" "$(stat -c %s /config.json)" "$layer" "$(stat -c %s /layer.tar)" >/manifest.json \
 && digest=$(sha256sum /manifest.json | cut -d' ' -f1) \
 && cp /layer.tar /oci/blobs/sha256/$layer && cp /config.json /oci/blobs/sha256/$config && cp /manifest.json /oci/blobs/sha256/$digest \
 && printf '{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:%s","size":%s,"annotations":{"io.containerd.image.name":"naru.local/coredns:release"}}]}' \
      "$digest" "$(stat -c %s /manifest.json)" >/oci/index.json \
 && printf '{"imageLayoutVersion":"1.0.0"}' >/oci/oci-layout \
 && tar --sort=name --mtime="@$SOURCE_DATE_EPOCH" --owner=0 --group=0 --numeric-owner --format=gnu -cf /out/usr/lib/naru/addons/images/coredns.tar -C /oci . \
 && sed "s#@COREDNS_IMAGE@#naru.local/coredns@sha256:$digest#" /dns.yaml.in >/out/usr/lib/naru/addons/manifests/dns.yaml \
 && ! grep -q '@' /out/usr/lib/naru/addons/manifests/dns.yaml

FROM scratch
COPY --from=build /out /
