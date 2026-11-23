# The Guest Agent's initrd and the converter's initrd (ADR 0010, 0075), built with mvmd's own Makefile
# in the pinned mvmd checkout, and each checked against the SHA-256 of pin.env: the same sources give
# the same bytes, which is what makes them parts of the image and not inputs a test supplies. The Rust
# toolchain is the one mvmd's rust-toolchain.toml names, which rustup installs (the builder carries
# only rustup-init, pinned by SHA-256), and erofs-utils builds itself.
FROM builder AS build
SHELL ["/bin/bash", "-euo", "pipefail", "-c"]
ARG SOURCE_DATE_EPOCH
ARG MVMD_COMMIT
ENV TZ=UTC LC_ALL=C SOURCE_DATE_EPOCH=${SOURCE_DATE_EPOCH} \
    GOPATH=/go GOCACHE=/gocache CARGO_HOME=/cargo RUSTUP_HOME=/rustup \
    PATH=/cargo/bin:/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
COPY --from=lib gitfetch.sh /usr/local/bin/
COPY --from=mvmd-pin pin.env /mvmd-pin.env
COPY pin.env /pin.env
WORKDIR /build
RUN . /mvmd-pin.env && : "${MVMD_COMMIT:?build.sh passes the commit the Go modules pin}" && gitfetch.sh "$MVMD_REPO" "$MVMD_COMMIT" src
RUN rustup-init -y --default-toolchain none --profile minimal --no-modify-path \
 && for dir in guest-agent converter; do \
      (cd "src/$dir" && rustup toolchain install --no-self-update --profile minimal && rustc --version); \
    done
RUN make -C src guest-initrd converter-initrd
RUN . /pin.env && mkdir -p /out \
 && cp src/guest-agent/target/initrd.cpio /out/guest-initrd.cpio \
 && cp src/converter/target/initrd.cpio /out/converter-initrd.cpio \
 && for v in guest-initrd:$GUEST_INITRD_SHA256 converter-initrd:$CONVERTER_INITRD_SHA256; do \
      got=$(sha256sum "/out/${v%%:*}.cpio" | cut -d' ' -f1); \
      [ "$got" = "${v##*:}" ] || { echo "guest-initrds: ${v%%:*}.cpio is $got at mvmd $MVMD_COMMIT, not the ${v##*:} of pin.env" >&2; exit 1; }; \
    done

FROM scratch
COPY --from=build /out /
