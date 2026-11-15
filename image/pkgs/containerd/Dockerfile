# containerd, its runc shim and runc (ADR 0026, 0075), static, from the pinned commits. containerd and
# the shim are pure Go, with the tags that keep the btrfs, zfs and devmapper snapshotters out. runc
# links libseccomp through cgo, so it is compiled and linked by the toolchain, statically, against
# the libseccomp package; netgo and osusergo keep it from reading a libc name-service module. A
# build that cannot link libseccomp fails: no runc without seccomp is made. The Go release is the
# one containerd's go.mod names, and runc is built with it too (its go.mod asks for an older one).
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
COPY --from=libseccomp /usr /toolchain/x86_64-linux-musl/usr
ENV GOPATH=/go GOCACHE=/gocache GOFLAGS=-mod=readonly GOWORK=off GOOS=linux GOARCH=amd64
COPY --from=lib gitfetch.sh install-go.sh install-bin.sh /usr/local/bin/
COPY pin.env /pin.env
WORKDIR /build
RUN . /pin.env && gitfetch.sh "$CONTAINERD_REPO" "$CONTAINERD_COMMIT" containerd && gitfetch.sh "$RUNC_REPO" "$RUNC_COMMIT" runc
RUN . /pin.env && export GOTOOLCHAIN=go$(awk '$1 == "go" { print $2 }' containerd/go.mod) && [ "$GOTOOLCHAIN" != go ] \
 && pkg=github.com/containerd/containerd/v2 \
 && ldflags="-s -w -buildid= -X $pkg/version.Version=$CONTAINERD_VERSION -X $pkg/version.Revision=$CONTAINERD_COMMIT -X $pkg/version.Package=$pkg" \
 && tags="urfave_cli_no_docs osusergo netgo static_build no_btrfs no_devmapper no_zfs" \
 && (cd containerd \
      && CGO_ENABLED=0 go build -trimpath -buildvcs=false -tags "$tags" -ldflags="$ldflags" -o /build/containerd ./cmd/containerd \
      && CGO_ENABLED=0 go build -trimpath -buildvcs=false -tags "$tags no_grpc" -ldflags="$ldflags" -o /build/containerd-shim-runc-v2 ./cmd/containerd-shim-runc-v2) \
 && for p in containerd containerd-shim-runc-v2; do install-go.sh "$GOTOOLCHAIN" /build/$p /out/usr/bin/$p; done \
 && [ "$(/build/containerd --version | awk '{ print $3 }')" = "$CONTAINERD_VERSION" ] \
 && [ "$(/build/containerd-shim-runc-v2 -v | awk '$1 == "Version:" { print $2 }')" = "$CONTAINERD_VERSION" ]
# runc: libpathrs, which runc's Makefile adds by default, is a Rust library the image does not
# build; without the tag runc uses its pure Go path-safety code.
RUN . /pin.env && export GOTOOLCHAIN=go$(awk '$1 == "go" { print $2 }' containerd/go.mod) \
 && (cd runc && CGO_ENABLED=1 PKG_CONFIG="pkg-config --static" CGO_CFLAGS="$CFLAGS" \
      go build -trimpath -buildvcs=false -tags seccomp,urfave_cli_no_docs,netgo,osusergo \
      -ldflags="-buildid= -linkmode=external -extldflags \"-static -Wl,--build-id=none\" -X main.gitCommit=$RUNC_COMMIT" \
      -o /build/runc .) \
 && install-go.sh "$GOTOOLCHAIN" /build/runc /build/runc.checked \
 && install-bin.sh /build/runc /out/usr/bin/runc \
 && runc_version=$(/out/usr/bin/runc --version) \
 && [ "$(echo "$runc_version" | awk 'NR == 1 { print $3 }')" = "$RUNC_VERSION" ] \
 && [ "$(echo "$runc_version" | awk '$1 == "libseccomp:" { print $2 }')" = "$LIBSECCOMP_VERSION" ]

FROM scratch
COPY --from=build /out /
