# xfsprogs (ADR 0027, 0075): the static mkfs.xfs, over libuuid and libblkid (util-linux), liburcu and
# libinih. Compiled by the toolchain.
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
COPY --from=util-linux /usr /toolchain/x86_64-linux-musl/usr
COPY --from=liburcu /usr /toolchain/x86_64-linux-musl/usr
COPY --from=libinih /usr /toolchain/x86_64-linux-musl/usr
COPY --from=lib fetch.sh install-bin.sh /usr/local/bin/
COPY pin.env /pin.env
WORKDIR /build
RUN . /pin.env && fetch.sh "$XFSPROGS_URL" "$XFSPROGS_SHA256" src.tar.xz && tar -xf src.tar.xz
# The tree's makefile discards the LDFLAGS of the environment and of configure, so the link flags are
# given to make (libtool takes -all-static for a static executable); the header directory is set up first.
# CXX=true: libxfs compiles one C++ file only to check that its headers are usable from C++, and links
# nothing of it, while the toolchain is C only.
RUN . /pin.env && cd xfsprogs-$XFSPROGS_VERSION \
 && ./configure --disable-shared --enable-scrub=no --enable-editline=no --enable-lib64=no \
      --enable-gettext=no CXXFLAGS="$CFLAGS" LDFLAGS="-static -Wl,--build-id=none" \
 && make headers && make -j"$(nproc)" CXX=true LDFLAGS="-all-static -Wl,--build-id=none" mkfs \
 && install-bin.sh mkfs/mkfs.xfs /out/usr/sbin/mkfs.xfs

FROM scratch
COPY --from=build /out /
