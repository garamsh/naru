module github.com/garamsh/naru/host

go 1.26.0

require (
	github.com/garamsh/mvmd/lib/runtimeapi v0.0.0-20261114001926-191326aca748
	github.com/stretchr/testify v1.12.1
	google.golang.org/grpc v1.85.0
	google.golang.org/protobuf v1.36.12
)

require (
	github.com/garamsh/naru/lib/iso9660 v0.0.0-00010101000000-000000000000
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	golang.org/x/net v0.60.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/text v0.42.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260817212433-ac3dfec99bb1 // indirect
)

replace github.com/garamsh/naru/lib/iso9660 => ../lib/iso9660
