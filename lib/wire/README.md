# lib/wire

The values naru's units agree on by name and not by call: the data disk's label and mount, the EFI system partition's GUID, the mkfs path the image carries, the ports of the APIs and of etcd, the form of an enrollment key, the header of orch's fencing token and the protocols of an exposed port.

Linked by `lib/hostapi`, `lib/init`, `lib/layout`, `lib/orchapi`, `host`, `node`, `orch`, `naructl` and `tooling/qemu`. It imports nothing, so a module that needs one value does not take a library with it (ADR 0070).
