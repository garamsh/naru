# The Guest Agent's initrd with 512 zero bytes after its cpio trailer, for the upgrade test of a
# changed guest manifest (ADR 0106): the kernel's unpacker skips zeros between and after archive
# members, so the initrd is as good a guest as the image's own, and its SHA-256 differs, so the
# guest.json of an image built on it names another digest. The converter's initrd is the image's own.
FROM builder AS build
SHELL ["/bin/bash", "-euo", "pipefail", "-c"]
COPY --from=guest-initrds /guest-initrd.cpio /converter-initrd.cpio /in/
RUN mkdir /out \
 && { cat /in/guest-initrd.cpio; head -c 512 /dev/zero; } >/out/guest-initrd.cpio \
 && cp /in/converter-initrd.cpio /out/converter-initrd.cpio

FROM scratch
COPY --from=build /out /
