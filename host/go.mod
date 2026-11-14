module github.com/garamsh/naru/host

go 1.26.0

require (
	github.com/garamsh/mvmd/lib/runtimeapi v0.0.0-20261114055347-b1a60cd64967
	github.com/prometheus/client_golang v1.25.0
	github.com/stretchr/testify v1.12.1
	google.golang.org/grpc v1.85.0
	google.golang.org/protobuf v1.36.12
)

require golang.org/x/crypto v0.57.0 // indirect

require (
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/prometheus/client_model v0.6.3 // indirect
	github.com/prometheus/common v0.72.0 // indirect
	github.com/prometheus/procfs v0.22.0 // indirect
)

require (
	github.com/garamsh/naru/lib/hostapi v0.0.0-00010101000000-000000000000
	github.com/garamsh/naru/lib/identity v0.0.0-00010101000000-000000000000
	github.com/garamsh/naru/lib/iso9660 v0.0.0-00010101000000-000000000000
	github.com/garamsh/naru/lib/metrics v0.0.0-00010101000000-000000000000
	github.com/garamsh/naru/lib/orchapi v0.0.0-00010101000000-000000000000
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	golang.org/x/net v0.60.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
	golang.org/x/text v0.42.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260817212433-ac3dfec99bb1 // indirect
)

replace github.com/garamsh/naru/lib/identity => ../lib/identity

replace github.com/garamsh/naru/lib/hostapi => ../lib/hostapi

replace github.com/garamsh/naru/lib/iso9660 => ../lib/iso9660

replace github.com/garamsh/naru/lib/orchapi => ../lib/orchapi

replace github.com/garamsh/naru/lib/metrics => ../lib/metrics

replace github.com/garamsh/naru/lib/init => ../lib/init

require github.com/garamsh/naru/lib/init v0.0.0-00010101000000-000000000000

require (
	github.com/vishvananda/netlink v1.3.1 // indirect
	github.com/vishvananda/netns v0.0.5 // indirect
	go.yaml.in/yaml/v4 v4.0.0 // indirect
)
