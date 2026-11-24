# The Host's initramfs (ADR 0020, 0075): initramfs.cpio.gz, which the kernel embeds, laid out by
# initramfs.list. Besides the Host's programs it carries what a Machine boots on and what the Host
# starts a Node from, under GUEST_IMAGE_DIR (kernels.env, boot-set.env): each guest kernel, the
# Guest Agent's and the converter's initrds, the Node's kernel and initramfs as the boot set
# GUEST_BOOT_SET (ADR 0054), and guest.json and boot-sets.json, which name each file with the path it
# has inside the image and its SHA-256 (mvmd's driver configuration, mvmd ADR 0058). The package
# holds guest.json and the files it names too, under /guest, for the QEMU tests, and boot-sets.json.
# naru-init is built here, for the version NARU_VERSION and the linker flags EXTRA_LDFLAGS, as the
# Node's initramfs does. The archive is made as the Node's is.
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
 && cd /src/host && go build -trimpath -buildvcs=false \
      -ldflags="-s -w -X github.com/garamsh/naru/lib/init/run.buildVersion=$NARU_VERSION $EXTRA_LDFLAGS" \
      -o /tmp/naru-init ./cmd/naru-init \
 && install-go.sh "$NARU_GO_VERSION" /tmp/naru-init /stage/naru-init
COPY --from=naru /usr/bin/naru-standin /usr/bin/naru-vip /usr/bin/naru-fence /usr/bin/naru-hostd /stage/
COPY --from=mvmd /usr/bin/mvmd /usr/bin/mvmd-supervisor /usr/bin/firecracker /stage/
COPY --from=gaon /usr/bin/gaon-node-agent /usr/bin/gaon-apiserver /usr/bin/gaon-scheduler /usr/bin/gaon-controllers /stage/
COPY --from=e2fsprogs /usr/sbin/mke2fs /stage/mkfs.ext4
COPY --from=xfsprogs /usr/sbin/mkfs.xfs /stage/
COPY --from=nftables /usr/sbin/nft /stage/
COPY --from=etcd /usr/bin/etcd /stage/
COPY --from=ecr-login /usr/bin/docker-credential-ecr-login /stage/
COPY --from=rolesanywhere /usr/bin/aws_signing_helper /stage/
COPY --from=cacert /etc/ssl/certs/ca-certificates.crt /cacert.crt
COPY --from=testroot / /testroot/
# The guest files and the Node's boot set, as the package lays them out under /out/guest.
COPY --from=kernel-guest-naru /vmlinux /guest/kernels/naru/vmlinux
COPY --from=kernel-node /vmlinux /guest/boot-sets/naru-node/vmlinux
COPY --from=guest-initrds /guest-initrd.cpio /converter-initrd.cpio /guest/
COPY --from=initramfs-node /node-initrd.cpio.gz /node-initrd.cpio.gz
COPY kernels.env boot-set.env initramfs.list /
RUN { cat /cacert.crt; if [ -f /testroot/ca.pem ]; then cat /testroot/ca.pem; fi; } >/stage/ca-certificates.crt \
 && for f in naru-init mvmd mvmd-supervisor firecracker mkfs.ext4 mkfs.xfs naru-standin naru-vip naru-fence naru-hostd \
      gaon-node-agent gaon-apiserver gaon-scheduler gaon-controllers etcd docker-credential-ecr-login aws_signing_helper; do \
      file "/stage/$f" | grep -Eq 'statically linked|static-pie linked' || { echo "initramfs: $f is not a static executable" >&2; exit 1; }; \
    done
COPY assemble.sh /assemble.sh
RUN /assemble.sh

FROM scratch
COPY --from=build /out /
