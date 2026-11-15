# e2fsprogs (ADR 0011, 0075): the static mke2fs, which the initramfs carries as mkfs.ext4.
FROM builder AS build
SHELL ["/bin/bash", "-euo", "pipefail", "-c"]
ARG SOURCE_DATE_EPOCH
ENV TZ=UTC LC_ALL=C SOURCE_DATE_EPOCH=${SOURCE_DATE_EPOCH}
COPY --from=toolchain /toolchain /toolchain
ENV SYSROOT=/toolchain/x86_64-linux-musl PATH=/toolchain/bin:${PATH} \
    CC=x86_64-linux-musl-gcc AR=x86_64-linux-musl-ar RANLIB=x86_64-linux-musl-ranlib \
    STRIP=x86_64-linux-musl-strip CFLAGS="-O2 -g0 -ffile-prefix-map=/build=." \
    PKG_CONFIG_LIBDIR=/toolchain/x86_64-linux-musl/usr/lib/pkgconfig \
    PKG_CONFIG_SYSROOT_DIR=/toolchain/x86_64-linux-musl
COPY --from=lib fetch.sh install-bin.sh /usr/local/bin/
COPY pin.env /pin.env
WORKDIR /build
RUN . /pin.env && fetch.sh "$E2FSPROGS_URL" "$E2FSPROGS_SHA256" src.tar.xz && tar -xf src.tar.xz
RUN . /pin.env && mkdir obj && cd obj \
 && ../e2fsprogs-$E2FSPROGS_VERSION/configure \
      --disable-nls --disable-fuse2fs --disable-debugfs --disable-imager --disable-resizer \
      --disable-defrag --disable-uuidd --disable-e2initrd-helper --disable-elf-shlibs \
      --disable-bsd-shlibs --disable-rpath --disable-testio-debug \
      --enable-libuuid --enable-libblkid LDFLAGS="-static -Wl,--build-id=none" \
 && make -j"$(nproc)" libs && make -C misc -j"$(nproc)" mke2fs \
 && install-bin.sh misc/mke2fs /out/usr/sbin/mke2fs

FROM scratch
COPY --from=build /out /
