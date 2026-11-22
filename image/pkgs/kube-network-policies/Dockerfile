# kube-network-policies (ADR 0086, ADR 0075): the NetworkPolicy enforcer, a static binary built from
# the pinned commit. No version stamping: the binary is identified by the commit it was built from.
FROM builder AS build
SHELL ["/bin/bash", "-euo", "pipefail", "-c"]
ARG SOURCE_DATE_EPOCH
ENV TZ=UTC LC_ALL=C SOURCE_DATE_EPOCH=${SOURCE_DATE_EPOCH} \
    GOPATH=/go GOCACHE=/gocache GOFLAGS=-mod=readonly GOWORK=off \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64
COPY --from=lib gitfetch.sh install-go.sh /usr/local/bin/
COPY pin.env /pin.env
WORKDIR /build
RUN . /pin.env && gitfetch.sh "$KNP_REPO" "$KNP_COMMIT" src
RUN . /pin.env && cd src \
 && export GOTOOLCHAIN=go$(awk '$1 == "go" { print $2 }' go.mod) && [ "$GOTOOLCHAIN" != go ] \
 && go build -trimpath -buildvcs=false -ldflags="-s -w -buildid=" \
      -o /build/kube-network-policies ./cmd/kube-network-policies \
 && install-go.sh "$GOTOOLCHAIN" /build/kube-network-policies /out/usr/bin/kube-network-policies

FROM scratch
COPY --from=build /out /
