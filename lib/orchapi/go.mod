module github.com/garamsh/naru/lib/orchapi

go 1.26.0

require (
	github.com/garamsh/naru/lib/identity v0.0.0-00010101000000-000000000000
	github.com/stretchr/testify v1.12.1
)

require (
	github.com/garamsh/naru/lib/atomicfile v0.0.0-00010101000000-000000000000 // indirect
	github.com/garamsh/naru/lib/machineapi v0.0.0-00010101000000-000000000000
	github.com/garamsh/naru/lib/wire v0.0.0-00010101000000-000000000000
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	golang.org/x/crypto v0.58.0 // indirect
)

replace github.com/garamsh/naru/lib/identity => ../identity

replace github.com/garamsh/naru/lib/atomicfile => ../atomicfile

replace github.com/garamsh/naru/lib/wire => ../wire

replace github.com/garamsh/naru/lib/machineapi => ../machineapi
