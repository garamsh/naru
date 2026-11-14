module github.com/garamsh/naru/node

go 1.26.0

require (
	github.com/google/nftables v0.3.0
	github.com/mdlayher/netlink v1.11.2
	github.com/stretchr/testify v1.12.1
	github.com/vishvananda/netlink v1.3.1 // indirect
	golang.org/x/sys v0.49.0
)

require (
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/garamsh/naru/lib/metrics v0.0.0-00010101000000-000000000000 // indirect
	github.com/google/go-cmp v0.7.0 // indirect
	github.com/mdlayher/socket v0.6.0 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/prometheus/client_golang v1.25.0 // indirect
	github.com/prometheus/client_model v0.6.3 // indirect
	github.com/prometheus/common v0.72.0 // indirect
	github.com/prometheus/procfs v0.22.0 // indirect
	github.com/vishvananda/netns v0.0.5 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	go.yaml.in/yaml/v4 v4.0.0 // indirect
	golang.org/x/net v0.59.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	google.golang.org/protobuf v1.36.12 // indirect
)

replace (
	github.com/garamsh/naru/lib/init => ../lib/init
	github.com/garamsh/naru/lib/metrics => ../lib/metrics
	github.com/garamsh/naru/lib/vip => ../lib/vip
)

require (
	github.com/garamsh/naru/lib/init v0.0.0-00010101000000-000000000000
	github.com/garamsh/naru/lib/vip v0.0.0-00010101000000-000000000000
)
