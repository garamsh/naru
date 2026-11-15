# libseccomp (ADR 0026, 0075) as a static library for runc to link, compiled by the toolchain. Its
# build runs gperf to generate the syscall tables, so gperf is built first, into a prefix of its own
# that stays out of the package.
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
COPY --from=lib fetch.sh /usr/local/bin/fetch.sh
COPY pin.env /pin.env
WORKDIR /build
RUN . /pin.env && fetch.sh "$GPERF_URL" "$GPERF_SHA256" gperf.tar.gz && fetch.sh "$LIBSECCOMP_URL" "$LIBSECCOMP_SHA256" src.tar.gz \
 && tar -xf gperf.tar.gz && tar -xf src.tar.gz
RUN . /pin.env && mkdir obj-gperf && cd obj-gperf \
 && ../gperf-$GPERF_VERSION/configure --prefix=/tools && make -j"$(nproc)" && make install
RUN . /pin.env && mkdir obj && cd obj \
 && PATH=/tools/bin:$PATH ../libseccomp-$LIBSECCOMP_VERSION/configure --prefix=/usr \
      --enable-static --disable-shared --disable-python --disable-tools \
 && PATH=/tools/bin:$PATH make -j"$(nproc)" && make DESTDIR=/out install \
 && find /out -name '*.la' -delete && rm -rf /out/usr/share /out/usr/bin \
 && [ -f /out/usr/lib/libseccomp.a ]

FROM scratch
COPY --from=build /out /
