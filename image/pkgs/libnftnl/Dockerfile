# libnftnl (ADR 0075): a static library for nftables, over libmnl, installed under /usr. Compiled by
# the toolchain.
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
COPY --from=libmnl /usr /toolchain/x86_64-linux-musl/usr
COPY --from=lib fetch.sh /usr/local/bin/fetch.sh
COPY pin.env /pin.env
WORKDIR /build
RUN . /pin.env && fetch.sh "$LIBNFTNL_URL" "$LIBNFTNL_SHA256" src.tar.xz && tar -xf src.tar.xz
RUN . /pin.env && mkdir obj && cd obj \
 && ../libnftnl-$LIBNFTNL_VERSION/configure --prefix=/usr --disable-shared --enable-static \
 && make -j"$(nproc)" && make DESTDIR=/out install \
 && find /out -name '*.la' -delete && rm -rf /out/usr/share

FROM scratch
COPY --from=build /out /
