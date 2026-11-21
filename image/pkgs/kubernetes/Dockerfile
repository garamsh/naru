# kube-apiserver, kube-controller-manager, kube-scheduler, kubelet, kube-proxy and kubectl (ADR 0075, 0083), static,
# from the pinned commit, with the version stamped the way Kubernetes' own build does. The Go release
# is the one the checkout's .go-version names, which is what Kubernetes builds that tag with.
FROM builder AS build
SHELL ["/bin/bash", "-euo", "pipefail", "-c"]
ARG SOURCE_DATE_EPOCH
ENV TZ=UTC LC_ALL=C SOURCE_DATE_EPOCH=${SOURCE_DATE_EPOCH} \
    GOPATH=/go GOCACHE=/gocache GOFLAGS=-mod=readonly GOWORK=off \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64
COPY --from=lib gitfetch.sh install-go.sh /usr/local/bin/
COPY pin.env /pin.env
WORKDIR /build
RUN . /pin.env && gitfetch.sh "$KUBERNETES_REPO" "$KUBERNETES_COMMIT" src
RUN . /pin.env && cd src \
 && export GOTOOLCHAIN=go$(tr -d '[:space:]' <.go-version) && [ "$GOTOOLCHAIN" != go ] \
 && build_date=$(date -u --date="@$SOURCE_DATE_EPOCH" +%Y-%m-%dT%H:%M:%SZ) \
 && minor=${KUBERNETES_VERSION#v*.} && minor=${minor%%.*} \
 && ldflags="-s -w -buildid=" \
 && for pkg in k8s.io/client-go/pkg/version k8s.io/component-base/version; do \
      ldflags="$ldflags -X $pkg.gitVersion=$KUBERNETES_VERSION -X $pkg.gitCommit=$KUBERNETES_COMMIT -X $pkg.gitTreeState=clean"; \
      ldflags="$ldflags -X $pkg.gitMajor=1 -X $pkg.gitMinor=$minor -X $pkg.buildDate=$build_date"; \
    done \
 && programs="kube-apiserver kube-controller-manager kube-scheduler kubelet kube-proxy kubectl" \
&& mkdir -p /build/bin \
 && go build -trimpath -buildvcs=false -tags selinux,notest,grpcnotrace -ldflags="$ldflags" \
      -o /build/bin/ $(for p in $programs; do echo ./cmd/$p; done) \
 && for p in $programs; do \
      install-go.sh "$GOTOOLCHAIN" /build/bin/$p /out/usr/bin/$p; \
      if [ "$p" = kubectl ]; then v=$(/build/bin/$p version --client | awk '$1 == "Client" && $2 == "Version:" { print $3 }'); else v=$(/build/bin/$p --version | awk '{ print $NF }'); fi; \
      [ "$v" = "$KUBERNETES_VERSION" ]; \
    done

FROM scratch
COPY --from=build /out /
