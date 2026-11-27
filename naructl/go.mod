module github.com/garamsh/naru/naructl

go 1.26.0

require (
	github.com/garamsh/naru/lib/hostapi v0.0.0-00010101000000-000000000000
	github.com/garamsh/naru/lib/identity v0.0.0-00010101000000-000000000000
	github.com/garamsh/naru/lib/iso9660 v0.0.0-00010101000000-000000000000
	github.com/garamsh/naru/lib/layout v0.0.0-00010101000000-000000000000
	github.com/garamsh/naru/lib/orchapi v0.0.0-00010101000000-000000000000
	github.com/spf13/cobra v1.10.2
	github.com/spf13/pflag v1.0.10
	github.com/stretchr/testify v1.12.1
	go.yaml.in/yaml/v3 v3.0.5
)

require github.com/garamsh/naru/lib/machineapi v0.0.0-00010101000000-000000000000

require (
	github.com/garamsh/naru/lib/atomicfile v0.0.0-00010101000000-000000000000 // indirect
	github.com/garamsh/naru/lib/wire v0.0.0-00010101000000-000000000000
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	golang.org/x/crypto v0.58.0 // indirect
)

replace github.com/garamsh/naru/lib/hostapi => ../lib/hostapi

replace github.com/garamsh/naru/lib/identity => ../lib/identity

replace github.com/garamsh/naru/lib/iso9660 => ../lib/iso9660

replace github.com/garamsh/naru/lib/layout => ../lib/layout

replace github.com/garamsh/naru/lib/orchapi => ../lib/orchapi

replace github.com/garamsh/naru/lib/atomicfile => ../lib/atomicfile

replace github.com/garamsh/naru/lib/wire => ../lib/wire

replace github.com/garamsh/naru/lib/init => ../lib/init

replace github.com/garamsh/naru/lib/machineapi => ../lib/machineapi
