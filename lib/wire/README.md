# lib/wire

The values naru's units agree on by name and not by call: the data disk's label and mount, the EFI system partition's GUID, the mkfs path the image carries, the ports of the APIs and of etcd, the form of an enrollment key and the header of orch's fencing token.

Linked by `lib/init`, `lib/layout`, `host`, `node`, `orch`, `naructl` and `tooling/qemu`. It imports nothing, so a module that needs one value does not take a library with it (ADR 0070).
