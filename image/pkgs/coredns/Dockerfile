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
COPY --from=lib gitfetch.sh install-go.sh addon-image.sh /usr/local/bin/
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
# The image: one uncompressed layer with /coredns, in an OCI layout whose index names it for containerd
# (io.containerd.image.name) by the reference the manifests use. addon-image.sh makes the archive and
# prints that reference, which is written into dns.yaml and checked against the archive.
RUN mkdir -p /rootfs /out/usr/lib/naru/addons/images /out/usr/lib/naru/addons/manifests \
 && install -m 0755 /build/coredns /rootfs/coredns \
 && ref=$(addon-image.sh build coredns coredns /rootfs /out/usr/lib/naru/addons/images/coredns.tar) \
 && sed "s#@COREDNS_IMAGE@#$ref#" /dns.yaml.in >/out/usr/lib/naru/addons/manifests/dns.yaml \
 && ! grep -q '@[A-Z_]*@' /out/usr/lib/naru/addons/manifests/dns.yaml \
 && addon-image.sh check coredns /out/usr/lib/naru/addons/images/coredns.tar /out/usr/lib/naru/addons/manifests/dns.yaml

FROM scratch
COPY --from=build /out /
