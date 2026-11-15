# The CNI plugins bridge, host-local and loopback (ADR 0075), static, from the pinned commit. The Go
# release is the one the module's go.mod names.
FROM builder AS build
SHELL ["/bin/bash", "-euo", "pipefail", "-c"]
ARG SOURCE_DATE_EPOCH
ENV TZ=UTC LC_ALL=C SOURCE_DATE_EPOCH=${SOURCE_DATE_EPOCH} \
    GOPATH=/go GOCACHE=/gocache GOFLAGS=-mod=readonly GOWORK=off \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64
COPY --from=lib gitfetch.sh install-go.sh /usr/local/bin/
COPY pin.env /pin.env
WORKDIR /build
RUN . /pin.env && gitfetch.sh "$CNI_REPO" "$CNI_COMMIT" src
RUN . /pin.env && cd src \
 && export GOTOOLCHAIN=go$(awk '$1 == "go" { print $2 }' go.mod) && [ "$GOTOOLCHAIN" != go ] \
 && for p in bridge:plugins/main/bridge host-local:plugins/ipam/host-local loopback:plugins/main/loopback; do \
      name=${p%%:*}; \
      go build -trimpath -buildvcs=false \
        -ldflags="-s -w -buildid= -X github.com/containernetworking/plugins/pkg/utils/buildversion.BuildVersion=$CNI_VERSION" \
        -o /build/$name "./${p#*:}"; \
      install-go.sh "$GOTOOLCHAIN" /build/$name /out/usr/bin/$name; \
      version=$(env -u CNI_COMMAND /build/$name 2>&1 | grep -o "CNI $name plugin .*" | awk '{ print $4 }' || true); \
      [ "$version" = "$CNI_VERSION" ]; \
    done

FROM scratch
COPY --from=build /out /
