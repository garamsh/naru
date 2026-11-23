# util-linux (ADR 0075): libuuid, libblkid and libmount as static libraries for the packages that
# link them (xfsprogs). Compiled by the toolchain.
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
RUN . /pin.env && fetch.sh "$UTIL_LINUX_URL" "$UTIL_LINUX_SHA256" src.tar.xz && tar -xf src.tar.xz
RUN . /pin.env && mkdir obj && cd obj \
 && ../util-linux-$UTIL_LINUX_VERSION/configure --prefix=/usr \
      --disable-all-programs --enable-mount --enable-libmount --enable-libblkid --enable-libuuid \
      --enable-static-programs=mount,umount --disable-shared --enable-static \
      --disable-nls --disable-rpath --disable-makeinstall-chown --disable-bash-completion \
      --without-udev --without-systemd --without-selinux --without-ncurses --without-python \
      --without-tinfo --without-readline --without-libz --without-user --without-util \
      LDFLAGS="-Wl,--build-id=none" \
 && make -j"$(nproc)" && make mount.static umount.static \
 && make DESTDIR=/out install \
 && for p in mount umount; do \
      [ "$(./$p.static --version | awk 'NR == 1 { print $4 }')" = "$UTIL_LINUX_VERSION" ]; \
      install-bin.sh $p.static /out/usr/bin/$p; \
    done \
 && rm -rf /out/bin /out/sbin /out/usr/share /out/usr/sbin /out/usr/lib/*.la /out/etc

FROM scratch
COPY --from=build /out /
