# etcd (ADR 0012, 0075): the server, static, from the pinned commit. The Go release is the one the
# server module's go.mod names; the builder's go command downloads it when it is another one.
FROM builder AS build
SHELL ["/bin/bash", "-euo", "pipefail", "-c"]
ARG SOURCE_DATE_EPOCH
ENV TZ=UTC LC_ALL=C SOURCE_DATE_EPOCH=${SOURCE_DATE_EPOCH} \
    GOPATH=/go GOCACHE=/gocache GOFLAGS=-mod=readonly GOWORK=off \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64
COPY --from=lib gitfetch.sh install-go.sh /usr/local/bin/
COPY pin.env /pin.env
WORKDIR /build
RUN . /pin.env && gitfetch.sh "$ETCD_REPO" "$ETCD_COMMIT" src
RUN . /pin.env && cd src/server \
 && export GOTOOLCHAIN=$(awk '$1 == "toolchain" { print $2 }' go.mod) && [ -n "$GOTOOLCHAIN" ] \
 && go build -trimpath -buildvcs=false -ldflags='-s -w' -o /build/etcd . \
 && install-go.sh "$GOTOOLCHAIN" /build/etcd /out/usr/bin/etcd \
 && [ "$(/build/etcd --version | awk '/^etcd Version:/ { print $3 }')" = "$ETCD_VERSION" ]

FROM scratch
COPY --from=build /out /
