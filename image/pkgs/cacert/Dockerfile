# The public root bundle (ADR 0026, 0075), as /etc/ssl/certs/ca-certificates.crt. No compiler: the
# pinned file is the package. A test image appends its throwaway root to it afterwards (Makefile).
FROM builder AS build
COPY --from=lib fetch.sh /usr/local/bin/fetch.sh
COPY pin.env /pin.env
RUN . /pin.env && mkdir -p /out/etc/ssl/certs \
 && fetch.sh "$CACERT_URL" "$CACERT_SHA256" /out/etc/ssl/certs/ca-certificates.crt \
 && grep -q '^-----BEGIN CERTIFICATE-----$' /out/etc/ssl/certs/ca-certificates.crt

FROM scratch
COPY --from=build /out /
