# gaon's four services and its command line client (ADR 0012, 0075), static, from the pinned commit.
# The Go release is the one gaon's go.work names.  gaon is a workspace, so the
# build leaves GOWORK unset and takes the modules' versions from go.work.sum. The client is for the QEMU tests to run on the
# host; the initramfs names the four services and does not take it.
FROM builder AS build
SHELL ["/bin/bash", "-euo", "pipefail", "-c"]
ARG SOURCE_DATE_EPOCH
ENV TZ=UTC LC_ALL=C SOURCE_DATE_EPOCH=${SOURCE_DATE_EPOCH} \
    GOPATH=/go GOCACHE=/gocache GOFLAGS=-mod=readonly GOWORK=off \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64
COPY --from=lib gitfetch.sh install-go.sh /usr/local/bin/
COPY pin.env /pin.env
WORKDIR /build
RUN . /pin.env && gitfetch.sh "$GAON_REPO" "$GAON_COMMIT" src
ENV GOFLAGS= GOWORK=
RUN cd src && export GOTOOLCHAIN=$(awk '$1 == "toolchain" { print $2 }' go.work) && [ -n "$GOTOOLCHAIN" ] \
 && for pkg in node-agent/cmd/gaon-node-agent apiserver/cmd/gaon-apiserver scheduler/cmd/gaon-scheduler controllers/cmd/gaon-controllers gaon-cli/cmd/gaon; do \
      name=$(basename $pkg); \
      go build -trimpath -buildvcs=false -ldflags='-s -w' -o /build/$name "./$pkg"; \
      install-go.sh "$GOTOOLCHAIN" /build/$name /out/usr/bin/$name; \
    done

FROM scratch
COPY --from=build /out /
