# aws_signing_helper of AWS IAM Roles Anywhere (ADR 0026, 0075), static, from the pinned commit. The
# credential helper links cgo (its PKCS#11 support), so the toolchain compiles and links it statically
# against musl; netgo and osusergo keep it from reading a libc name-service module. GO_TOOLCHAIN of
# pin.env is the Go release.
FROM builder AS build
SHELL ["/bin/bash", "-euo", "pipefail", "-c"]
ARG SOURCE_DATE_EPOCH
ENV TZ=UTC LC_ALL=C SOURCE_DATE_EPOCH=${SOURCE_DATE_EPOCH}
COPY --from=toolchain /toolchain /toolchain
ENV PATH=/toolchain/bin:${PATH} CC=x86_64-linux-musl-gcc AR=x86_64-linux-musl-ar \
    CGO_CFLAGS="-O2 -g0 -ffile-prefix-map=/build=." \
    GOPATH=/go GOCACHE=/gocache GOFLAGS=-mod=readonly GOWORK=off GOOS=linux GOARCH=amd64
COPY --from=lib gitfetch.sh install-go.sh install-bin.sh /usr/local/bin/
COPY pin.env /pin.env
WORKDIR /build
RUN . /pin.env && gitfetch.sh "$ROLESANYWHERE_REPO" "$ROLESANYWHERE_COMMIT" src
RUN . /pin.env && export GOTOOLCHAIN=$GO_TOOLCHAIN && cd src \
 && CGO_ENABLED=1 go build -trimpath -buildvcs=false -tags netgo,osusergo \
      -ldflags="-buildid= -linkmode=external -extldflags '-static -Wl,--build-id=none' -X github.com/aws/rolesanywhere-credential-helper/cmd.Version=$ROLESANYWHERE_VERSION" \
      -o /build/aws_signing_helper main.go \
 && install-go.sh "$GOTOOLCHAIN" /build/aws_signing_helper /build/aws_signing_helper.checked \
 && install-bin.sh /build/aws_signing_helper /out/usr/bin/aws_signing_helper \
 && [ "$(/out/usr/bin/aws_signing_helper version | awk 'NR == 1 { print $NF }')" = "$ROLESANYWHERE_VERSION" ]

FROM scratch
COPY --from=build /out /
