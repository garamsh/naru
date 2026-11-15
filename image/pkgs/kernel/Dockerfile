# The kernels of the image (ADR 0020, 0054, 0075), one Dockerfile for all of them, selected by build
# arguments: the Host's kernel (KERNEL=host, BASE=host), the Node's (node, host) and the guest kernel
# (guest-naru, tinyconfig). BASE=host starts from the architecture's defconfig and merges the
# configuration shared by the Host and Node kernels (naru.config) and then the kernel's own
# (<KERNEL>/naru.config, if it has one); BASE=tinyconfig starts from tinyconfig and merges only the
# kernel's own. olddefconfig then resolves the result, and the build fails unless the final .config
# has a line matching each line of <KERNEL>/required.txt, so an option that a dependency turned off
# is not left out of the kernel unseen. The Host's kernel is a bzImage; the others are a vmlinux.
#
# Targets: `bare` is the kernel as it was built (bzImage.bare or vmlinux, and kernel.config) and
# `bzimage` is the Host's kernel with the Host's initramfs embedded (the `initramfs` context, which
# holds /initramfs.cpio.gz, ADR 0020): the same tree, built a second time with CONFIG_INITRAMFS_SOURCE.
# The sources are fetched and unpacked in a stage the kernels share.
FROM builder AS source
SHELL ["/bin/bash", "-euo", "pipefail", "-c"]
ARG SOURCE_DATE_EPOCH
ARG KBUILD_BUILD_TIMESTAMP
ENV TZ=UTC LC_ALL=C SOURCE_DATE_EPOCH=${SOURCE_DATE_EPOCH} \
    KBUILD_BUILD_TIMESTAMP=${KBUILD_BUILD_TIMESTAMP} KBUILD_BUILD_USER=naru \
    KBUILD_BUILD_HOST=naru KBUILD_BUILD_VERSION=1
COPY --from=lib fetch.sh /usr/local/bin/fetch.sh
COPY pin.env /pin.env
WORKDIR /build
RUN . /pin.env && fetch.sh "$KERNEL_URL" "$KERNEL_SHA256" linux.tar.xz \
 && tar -xf linux.tar.xz && mv "linux-$KERNEL_VERSION" linux && rm linux.tar.xz

FROM source AS configured
ARG KERNEL
ARG BASE
COPY naru.config /common.config
COPY ${KERNEL}/ /kernel/
RUN : "${KERNEL:?kernel: KERNEL names the kernel}" "${BASE:?kernel: BASE is host or tinyconfig}" \
 && mkdir obj \
 && case "$BASE" in \
      host) make -C linux O=/build/obj ARCH=x86_64 defconfig >/dev/null; fragments=(/common.config) ;; \
      tinyconfig) make -C linux O=/build/obj ARCH=x86_64 tinyconfig >/dev/null; fragments=() ;; \
      *) echo "kernel: BASE is $BASE, not tinyconfig or host" >&2; exit 1 ;; \
    esac \
 && if [ -f /kernel/naru.config ]; then fragments+=(/kernel/naru.config); fi \
 && (cd linux && scripts/kconfig/merge_config.sh -m -O /build/obj /build/obj/.config "${fragments[@]}" >/dev/null) \
 && make -C linux O=/build/obj ARCH=x86_64 olddefconfig >/dev/null \
 && missing=0 \
 && while IFS= read -r line; do \
      case "$line" in '' | '##'*) continue ;; esac; \
      if ! grep -Eqx -e "$line" /build/obj/.config; then \
        echo "kernel $KERNEL: the final .config has no line matching: $line" >&2; missing=$((missing + 1)); \
      fi; \
    done </kernel/required.txt \
 && if [ "$missing" -ne 0 ]; then echo "kernel $KERNEL: $missing required option(s) missing from the final .config" >&2; exit 1; fi \
 && echo "kernel $KERNEL: the final .config meets all $(grep -vc -e '^##' -e '^$' /kernel/required.txt) lines of required.txt"

FROM configured AS built
ARG KERNEL
ARG IMAGE_TARGET
RUN : "${IMAGE_TARGET:?kernel: IMAGE_TARGET is bzImage or vmlinux}" \
 && make -C linux O=/build/obj ARCH=x86_64 -j"$(nproc)" "$IMAGE_TARGET" >build.log 2>&1 \
    || { tail -n 40 build.log >&2; echo "kernel $KERNEL: the build failed" >&2; exit 1; } \
 && mkdir /out && install -m 0644 obj/.config /out/kernel.config \
 && if [ "$IMAGE_TARGET" = bzImage ]; then install -m 0644 obj/arch/x86/boot/bzImage /out/bzImage.bare; \
    else install -m 0644 obj/vmlinux /out/vmlinux; fi

FROM scratch AS bare
COPY --from=built /out /

FROM built AS embedded
COPY --from=initramfs /initramfs.cpio.gz /initramfs.cpio.gz
RUN linux/scripts/config --file obj/.config --set-str INITRAMFS_SOURCE /initramfs.cpio.gz \
 && make -C linux O=/build/obj ARCH=x86_64 olddefconfig >/dev/null \
 && grep -Fqx 'CONFIG_INITRAMFS_SOURCE="/initramfs.cpio.gz"' obj/.config \
 && make -C linux O=/build/obj ARCH=x86_64 -j"$(nproc)" bzImage >embed.log 2>&1 \
    || { tail -n 40 embed.log >&2; echo "kernel: the embedding build failed" >&2; exit 1; } \
 && mkdir /embedded && install -m 0644 obj/arch/x86/boot/bzImage /embedded/bzImage

FROM scratch AS bzimage
COPY --from=embedded /embedded /

# gen_init_cpio, the kernel's own initramfs archiver (usr/gen_init_cpio.c of the pinned tarball), which
# the initramfs packages run.
FROM source AS gen-init-cpio-build
RUN gcc -O2 -o /gen_init_cpio linux/usr/gen_init_cpio.c

FROM scratch AS gen-init-cpio
COPY --from=gen-init-cpio-build /gen_init_cpio /
