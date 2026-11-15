# The cross toolchain x86_64-linux-musl, installed in /toolchain (ADR 0075): binutils, gcc (C only,
# static) and musl, built by the builder's gcc, the one distribution compiler left in the image. Every
# C package compiles with this and never with the builder's gcc. Each source is a pinned tarball
# checked by SHA-256, the Linux UAPI headers come from the pinned kernel tarball, and no step
# reaches the network but the fetches. The sysroot is /toolchain/x86_64-linux-musl, with musl's
# headers and libc.a under usr/; a package's dependencies are installed into the same sysroot.
FROM builder AS build
SHELL ["/bin/bash", "-euo", "pipefail", "-c"]
ARG SOURCE_DATE_EPOCH
ENV TZ=UTC LC_ALL=C SOURCE_DATE_EPOCH=${SOURCE_DATE_EPOCH}
COPY --from=lib fetch.sh /usr/local/bin/fetch.sh
COPY --from=kernel pin.env /kernel.env
COPY pin.env /pin.env
WORKDIR /build
RUN . /pin.env && . /kernel.env \
 && for s in BINUTILS GCC GMP MPFR MPC MUSL; do \
      url=${s}_URL sum=${s}_SHA256; \
      fetch.sh "${!url}" "${!sum}" "/build/${s}.tar"; \
    done \
 && fetch.sh "$KERNEL_URL" "$KERNEL_SHA256" /build/KERNEL.tar \
 && mkdir src && for s in BINUTILS GCC GMP MPFR MPC MUSL KERNEL; do tar -xf "$s.tar" -C src; done

ENV TARGET=x86_64-linux-musl PREFIX=/toolchain SYSROOT=/toolchain/x86_64-linux-musl
ENV PATH=/toolchain/bin:${PATH}
ENV CFLAGS="-O2 -g0 -ffile-prefix-map=/build=." CXXFLAGS="-O2 -g0 -ffile-prefix-map=/build=."

# binutils, deterministic archives so that two builds give the same libc.a and libgcc.a.
RUN . /pin.env && mkdir obj-binutils && cd obj-binutils \
 && ../src/binutils-$BINUTILS_VERSION/configure --target=$TARGET --prefix=$PREFIX --with-sysroot=$SYSROOT \
      --disable-nls --disable-multilib --disable-werror --disable-gprofng --disable-gdb --disable-sim \
      --enable-deterministic-archives \
 && make -j"$(nproc)" && make install-strip

# The kernel's UAPI headers and musl's headers, into the sysroot. The kernel's own header install
# needs rsync, which the builder does not have; `make headers` writes the same files into usr/include.
RUN . /kernel.env && cd src/linux-$KERNEL_VERSION \
 && make ARCH=x86_64 headers >/dev/null \
 && (cd usr/include && find . -name '*.h' | sort | cpio --quiet -pdm "$SYSROOT/usr/include")
RUN . /pin.env && mkdir obj-musl-headers && cd obj-musl-headers \
 && CC=gcc ../src/musl-$MUSL_VERSION/configure --prefix=/usr --target=$TARGET --disable-shared \
 && make DESTDIR=$SYSROOT install-headers

# gcc, then libgcc, then musl itself, which needs the compiler; gmp, mpfr and mpc build in the gcc tree.
RUN . /pin.env && cd src/gcc-$GCC_VERSION \
 && mv ../gmp-$GMP_VERSION gmp && mv ../mpfr-$MPFR_VERSION mpfr && mv ../mpc-$MPC_VERSION mpc \
 && cd /build && mkdir obj-gcc && cd obj-gcc \
 && ../src/gcc-$GCC_VERSION/configure --target=$TARGET --prefix=$PREFIX --with-sysroot=$SYSROOT --with-build-sysroot=$SYSROOT \
      --enable-languages=c --disable-bootstrap --disable-multilib --disable-nls --disable-shared \
      --disable-libsanitizer --disable-libquadmath --disable-libgomp --disable-libatomic \
      --disable-libssp --disable-libitm --disable-libvtv --disable-gnu-indirect-function \
      --enable-initfini-array --enable-tls --disable-werror \
 && make -j"$(nproc)" all-gcc && make install-strip-gcc \
 && make -j"$(nproc)" all-target-libgcc && make install-strip-target-libgcc
RUN . /pin.env && mkdir obj-musl && cd obj-musl \
 && CC=$TARGET-gcc AR=$TARGET-ar RANLIB=$TARGET-ranlib ../src/musl-$MUSL_VERSION/configure \
      --prefix=/usr --target=$TARGET --disable-shared \
 && make -j"$(nproc)" && make DESTDIR=$SYSROOT install

# What the toolchain is for: a program it links is static, names no distribution compiler, and runs.
RUN printf '#include <stdio.h>\nint main(void){puts("toolchain ok");return 0;}\n' >hello.c \
 && $TARGET-gcc -O2 hello.c -o hello && file hello | grep -Eq 'statically linked' \
 && ! readelf -p .comment hello | grep -q Ubuntu && [ "$(./hello)" = "toolchain ok" ] \
 && rm -rf /toolchain/share

FROM scratch
COPY --from=build /toolchain /toolchain
