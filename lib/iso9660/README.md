# lib/iso9660

Writes a small ISO 9660 volume with a Joliet tree: one flat root directory of files, which is what a NoCloud drive is. It needs no genisoimage, mkisofs or xorriso and no root.

- `iso9660.Write(label, files)` returns the volume image.
- A label is at most 16 characters; a file name is at most 64.

Linked by `tooling/qemu`, which builds the seed of a test Node, and by `host`, which builds the seed of a microVM made in the OS form.
