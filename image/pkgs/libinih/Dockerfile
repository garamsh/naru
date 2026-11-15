# inih (ADR 0027, 0075): one source file and one header, archived by hand into libinih.a, for xfsprogs.
# Compiled by the toolchain.
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
RUN . /pin.env && fetch.sh "$INIH_URL" "$INIH_SHA256" src.tar.gz && tar -xf src.tar.gz
RUN . /pin.env && cd inih-$INIH_VERSION \
 && $CC $CFLAGS -c ini.c -o /build/ini.o \
 && mkdir -p /out/usr/lib /out/usr/include \
 && $AR rcsD /out/usr/lib/libinih.a /build/ini.o \
 && cp ini.h /out/usr/include/ini.h

FROM scratch
COPY --from=build /out /
