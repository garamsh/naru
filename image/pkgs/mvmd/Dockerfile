# mvmd is a Go workspace, so its build leaves GOFLAGS and GOWORK unset and takes the modules from go.work.
# mvmd, its supervisor and the Firecracker binary (ADR 0010, 0075), from the pinned mvmd commit. The
# two programs are static Go. Firecracker comes from mvmd's own checked fetch,
# tooling/firecracker/fetch.sh --firecracker-only, which downloads the release tarball and verifies
# its digest and the binary's; it requests no guest kernel (naru builds its own, ADR 0010).
# /inputs.env is what that script writes, naming MVMD_FIRECRACKER and MVMD_FIRECRACKER_SHA256, which
# the QEMU tests read; its path is rewritten to where the package puts the binary.
FROM builder AS build
SHELL ["/bin/bash", "-euo", "pipefail", "-c"]
ARG SOURCE_DATE_EPOCH
ENV TZ=UTC LC_ALL=C SOURCE_DATE_EPOCH=${SOURCE_DATE_EPOCH} \
    GOPATH=/go GOCACHE=/gocache \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64
COPY --from=lib gitfetch.sh install-go.sh /usr/local/bin/
COPY pin.env /pin.env
WORKDIR /build
RUN . /pin.env && gitfetch.sh "$MVMD_REPO" "$MVMD_COMMIT" src
RUN cd src \
 && for p in mvmd mvmd-supervisor; do \
      go build -trimpath -buildvcs=false -ldflags='-s -w' -o /build/$p ./daemon/cmd/$p; \
      install-go.sh "$NARU_GO_VERSION" /build/$p /out/usr/bin/$p; \
    done
RUN src/tooling/firecracker/fetch.sh --firecracker-only /build/inputs >/dev/null \
 && install -m 0755 /build/inputs/firecracker /out/usr/bin/firecracker \
 && file /out/usr/bin/firecracker | grep -Eq 'statically linked|static-pie linked' \
 && sed "s#=/build/inputs/#=/usr/bin/#" /build/inputs/inputs.env >/out/inputs.env \
 && grep -q '^MVMD_FIRECRACKER_SHA256=' /out/inputs.env

FROM scratch
COPY --from=build /out /
