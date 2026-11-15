# userspace-rcu (ADR 0075): a static library for the packages that link it, installed under /usr. Compiled by
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
COPY --from=lib fetch.sh /usr/local/bin/fetch.sh
COPY pin.env /pin.env
WORKDIR /build
RUN . /pin.env && fetch.sh "$URCU_URL" "$URCU_SHA256" src.tar && tar -xf src.tar
RUN . /pin.env && mkdir obj && cd obj \
 && ../userspace-rcu-$URCU_VERSION/configure --prefix=/usr --disable-shared --enable-static  \
 && make SUBDIRS="include src" -j"$(nproc)" && make SUBDIRS="include src" DESTDIR=/out install \
 && find /out -name '*.la' -delete && rm -rf /out/usr/share /out/usr/bin

FROM scratch
COPY --from=build /out /
