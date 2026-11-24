# lib/wire

The values naru's units agree on by name and not by call: the data disk's label and mount, the EFI system partition's GUID, the mkfs path the image carries, the ports of the APIs and of etcd, the form of an enrollment key, the header of orch's fencing token, the protocols of an exposed port, what naru-hostd and naru-proxy share on a proxy host (the proxy's user, the routes and revision files, the health address and port 443) and the PROXY protocol v2 header both write.

Linked by `lib/hostapi`, `lib/init`, `lib/layout`, `lib/orchapi`, `host`, `node`, `orch`, `proxy`, `naructl` and `tooling/qemu`. It imports nothing, so a module that needs one value does not take a library with it (ADR 0070).
