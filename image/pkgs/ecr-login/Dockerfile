# docker-credential-ecr-login (ADR 0026, 0075), static, from the pinned commit; the module vendors its
# dependencies. GO_TOOLCHAIN of pin.env is the Go release.
FROM builder AS build
SHELL ["/bin/bash", "-euo", "pipefail", "-c"]
ARG SOURCE_DATE_EPOCH
ENV TZ=UTC LC_ALL=C SOURCE_DATE_EPOCH=${SOURCE_DATE_EPOCH} \
    GOPATH=/go GOCACHE=/gocache GOFLAGS=-mod=readonly GOWORK=off \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64
COPY --from=lib gitfetch.sh install-go.sh /usr/local/bin/
COPY pin.env /pin.env
WORKDIR /build
ENV GOFLAGS=-mod=vendor
RUN . /pin.env && gitfetch.sh "$ECR_LOGIN_REPO" "$ECR_LOGIN_COMMIT" src
RUN . /pin.env && cd src/ecr-login && export GOTOOLCHAIN=$GO_TOOLCHAIN \
 && pkg=github.com/awslabs/amazon-ecr-credential-helper/ecr-login \
 && go build -trimpath -buildvcs=false \
      -ldflags="-s -w -buildid= -X $pkg/version.Version=$ECR_LOGIN_VERSION -X $pkg/version.GitCommitSHA=$ECR_LOGIN_COMMIT" \
      -o /build/docker-credential-ecr-login ./cli/docker-credential-ecr-login \
 && install-go.sh "$GOTOOLCHAIN" /build/docker-credential-ecr-login /out/usr/bin/docker-credential-ecr-login \
 && [ "$(/build/docker-credential-ecr-login version | awk 'NR == 1 { print $NF }')" = "$ECR_LOGIN_VERSION" ]

FROM scratch
COPY --from=build /out /
