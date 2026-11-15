# The naru-orch system extension (ADR 0055, 0075): a squashfs of /bin/naru-orchd, /bin/naru-signer
# and /manifest.json, and naru-orch.json, its description. The same inputs give the same bytes: the
# programs are those of the naru package, and mksquashfs runs with -reproducible, zstd, one
# processor, root ownership and times fixed by SOURCE_DATE_EPOCH. NARU_VERSION is the version the
# extension is built for: init mounts only an image whose manifest names its own version.
FROM builder AS build
SHELL ["/bin/bash", "-euo", "pipefail", "-c"]
ARG SOURCE_DATE_EPOCH
ARG NARU_VERSION
ENV TZ=UTC LC_ALL=C SOURCE_DATE_EPOCH=${SOURCE_DATE_EPOCH}
RUN : "${NARU_VERSION:?extension: NARU_VERSION is the version the image is built for}"
COPY --from=naru /usr/bin/naru-signer /usr/bin/naru-orchd /tree/bin/
RUN name=naru-orch && tree=/tree \
 && chmod 0755 "$tree" "$tree/bin" "$tree/bin/naru-signer" "$tree/bin/naru-orchd" \
 && printf '{"name":"%s","version":"%s","services":["naru-orchd","naru-signer"]}\n' "$name" "$NARU_VERSION" >"$tree/manifest.json" \
 && chmod 0644 "$tree/manifest.json" \
 && mkdir -p /out/extensions \
 && mksquashfs "$tree" "/out/extensions/$name.sqfs" -comp zstd -reproducible -all-root -no-xattrs -noappend -processors 1 -quiet >/dev/null \
 && sha=$(sha256sum "/out/extensions/$name.sqfs" | cut -d' ' -f1) \
 && printf '{"name":"%s","version":"%s","sha256":"%s","services":["naru-orchd","naru-signer"]}\n' "$name" "$NARU_VERSION" "$sha" >"/out/extensions/$name.json"

FROM scratch
COPY --from=build /out /
