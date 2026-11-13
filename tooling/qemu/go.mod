module github.com/garamsh/naru/tooling/qemu

go 1.26.0

require github.com/stretchr/testify v1.12.1

require (
	github.com/garamsh/mvmd/lib/runtimeapi v0.0.0-20261112183926-d189133bba8d
	go.yaml.in/yaml/v3 v3.0.5
)

require (
	github.com/garamsh/naru/lib/iso9660 v0.0.0-00010101000000-000000000000
	golang.org/x/net v0.60.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/text v0.42.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260817212433-ac3dfec99bb1 // indirect
	google.golang.org/grpc v1.85.0 // indirect
	google.golang.org/protobuf v1.36.12 // indirect
)

replace github.com/garamsh/naru/lib/iso9660 => ../../lib/iso9660
