# nftables (ADR 0075): the static nft, with JSON output (nft -j), over libmnl, libnftnl and
# jansson. Compiled by the toolchain.
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
COPY --from=libnftnl /usr /toolchain/x86_64-linux-musl/usr
COPY --from=jansson /usr /toolchain/x86_64-linux-musl/usr
COPY --from=lib fetch.sh install-bin.sh /usr/local/bin/
COPY pin.env /pin.env
WORKDIR /build
RUN . /pin.env && fetch.sh "$NFTABLES_URL" "$NFTABLES_SHA256" src.tar.xz && tar -xf src.tar.xz
RUN . /pin.env && mkdir obj && cd obj \
 && ../nftables-$NFTABLES_VERSION/configure --prefix=/usr --disable-shared --enable-static \
      --without-cli --disable-python --disable-man-doc --with-mini-gmp --disable-debug --with-json \
      --without-xtables --sysconfdir=/etc LDFLAGS="-static -Wl,--build-id=none" \
 && make -j"$(nproc)" LDFLAGS="-all-static -Wl,--build-id=none" \
 && [ "$(./src/nft --version | awk '{ print $2 }')" = "v$NFTABLES_VERSION" ] \
 && install-bin.sh src/nft /out/usr/sbin/nft

FROM scratch
COPY --from=build /out /
