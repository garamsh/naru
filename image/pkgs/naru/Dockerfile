# naru's own programs (ADR 0075), static, built by the builder's Go from the repository (the `src`
# context, which /.dockerignore narrows to the Go workspace): the host's naru-hostd, the Node's naru-vip
# and naru-fence, the stand-in service the tests configure, the programs of the naru-orch extension
# (naru-signer, naru-orchd), and the two release tools naructl and naru-cluster. naru-init is not
# here: it carries the image's version, so the initramfs packages build it.
FROM builder AS build
SHELL ["/bin/bash", "-euo", "pipefail", "-c"]
ARG SOURCE_DATE_EPOCH
ENV TZ=UTC LC_ALL=C SOURCE_DATE_EPOCH=${SOURCE_DATE_EPOCH} \
    GOPATH=/go GOCACHE=/gocache GOFLAGS=-mod=readonly \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64
COPY --from=lib install-go.sh /usr/local/bin/
COPY --from=src / /src
WORKDIR /src
RUN for p in host:naru-hostd:cmd/naru-hostd node:naru-vip:cmd/naru-vip node:naru-fence:cmd/naru-fence \
             tooling/qemu:naru-standin:cmd/naru-standin tooling/qemu:naru-cluster:cmd/naru-cluster \
             host:naru-signer:cmd/naru-signer orch:naru-orchd:cmd/naru-orchd naructl:naructl:cmd/naructl; do \
      IFS=: read -r dir name pkg <<<"$p"; \
      (cd "$dir" && go build -trimpath -buildvcs=false -ldflags='-s -w' -o "/build/$name" "./$pkg"); \
      install-go.sh "$NARU_GO_VERSION" "/build/$name" "/out/usr/bin/$name"; \
    done

FROM scratch
COPY --from=build /out /
