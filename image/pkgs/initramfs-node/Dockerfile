# The Node's initramfs (ADR 0020, 0075): node-initrd.cpio.gz, an archive of the programs the Node runs,
# laid out by initramfs.list, which the Host's initramfs carries as the boot set it starts a Node
# from (ADR 0054). naru-init is built here, not taken from the naru package, since it reports the
# version the image is built for (NARU_VERSION) and EXTRA_LDFLAGS adds to the linker's flags: the
# upgrade tests' refuse variant sets the data disk's label to one that is absent, so a trial boot
# of it refuses (ADR 0025). The archive is made by the kernel's own gen_init_cpio with every time
# SOURCE_DATE_EPOCH and compressed with gzip -9n, which stores no name or time: the same inputs give
# the same bytes. The root bundle is the cacert package's, with the throwaway root of the `testroot`
# context appended when a test image has one (ADR 0026); a release has none. The volumes extension
# rides as the .sqfs file and its description, for init to stage into a blank Node's store (ADR 0096).
FROM builder AS build
SHELL ["/bin/bash", "-euo", "pipefail", "-c"]
ARG SOURCE_DATE_EPOCH
ARG NARU_VERSION
ARG EXTRA_LDFLAGS=
ENV TZ=UTC LC_ALL=C SOURCE_DATE_EPOCH=${SOURCE_DATE_EPOCH} \
    GOPATH=/go GOCACHE=/gocache GOFLAGS=-mod=readonly CGO_ENABLED=0 GOOS=linux GOARCH=amd64
COPY --from=lib install-go.sh /usr/local/bin/
COPY --from=gen-init-cpio /gen_init_cpio /usr/local/bin/gen_init_cpio
COPY --from=src / /src
RUN : "${NARU_VERSION:?initramfs: NARU_VERSION is the version the image is built for}" \
 && (cd /src/tooling/disk && go build -trimpath -buildvcs=false -o /usr/local/bin/naru-cpiolist ./cmd/naru-cpiolist) \
 && cd /src/node && go build -trimpath -buildvcs=false \
      -ldflags="-s -w -X github.com/garamsh/naru/lib/init/run.buildVersion=$NARU_VERSION $EXTRA_LDFLAGS" \
      -o /tmp/naru-init ./cmd/naru-init \
 && install-go.sh "$NARU_GO_VERSION" /tmp/naru-init /stage/naru-init
COPY --from=naru /usr/bin/naru-standin /usr/bin/naru-vip /usr/bin/naru-fence /stage/
COPY --from=e2fsprogs /usr/sbin/mke2fs /stage/mkfs.ext4
COPY --from=xfsprogs /usr/sbin/mkfs.xfs /stage/
COPY --from=nftables /usr/sbin/nft /stage/
COPY --from=util-linux /usr/bin/mount /usr/bin/umount /stage/
COPY --from=etcd /usr/bin/etcd /stage/
COPY --from=ecr-login /usr/bin/docker-credential-ecr-login /stage/
COPY --from=rolesanywhere /usr/bin/aws_signing_helper /stage/
COPY --from=kubernetes /usr/bin/ /stage/
COPY --from=kube-network-policies /usr/bin/kube-network-policies /stage/
COPY --from=containerd /usr/bin/ /stage/
COPY --from=cni /usr/bin/ /stage/
COPY --from=coredns /usr/lib/naru/addons/ /stage/addons/
COPY --from=ccm /usr/lib/naru/addons/ /stage/addons/
COPY --from=volumes /extensions/ /stage/extensions/
COPY --from=cacert /etc/ssl/certs/ca-certificates.crt /cacert.crt
COPY --from=testroot / /testroot/
COPY initramfs.list /initramfs.list
RUN { cat /cacert.crt; if [ -f /testroot/ca.pem ]; then cat /testroot/ca.pem; fi; } >/stage/ca-certificates.crt \
 && for f in naru-init mkfs.ext4 mkfs.xfs naru-standin naru-vip naru-fence etcd docker-credential-ecr-login aws_signing_helper \
      kube-apiserver kube-controller-manager kube-scheduler kubelet kube-proxy kube-network-policies kubectl ctr containerd containerd-shim-runc-v2 runc bridge host-local loopback; do \
      file "/stage/$f" | grep -Eq 'statically linked|static-pie linked' || { echo "initramfs: $f is not a static executable" >&2; exit 1; }; \
    done \
 && sed "s#@OUT@#/stage#" /initramfs.list >/node.raw.list \
 && naru-cpiolist order /node.raw.list >/node.list \
 && naru-cpiolist check /node.list \
 && mkdir /out && gen_init_cpio -t "$SOURCE_DATE_EPOCH" /node.list | gzip -9n >/out/node-initrd.cpio.gz

FROM scratch
COPY --from=build /out /
