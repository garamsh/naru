module github.com/garamsh/naru/proxy

go 1.26.0

replace (
	github.com/garamsh/naru/lib/atomicfile => ../lib/atomicfile
	github.com/garamsh/naru/lib/hostapi => ../lib/hostapi
	github.com/garamsh/naru/lib/wire => ../lib/wire
)

require (
	github.com/garamsh/naru/lib/atomicfile v0.0.0-00010101000000-000000000000
	github.com/garamsh/naru/lib/hostapi v0.0.0-00010101000000-000000000000
	github.com/garamsh/naru/lib/wire v0.0.0-00010101000000-000000000000
	github.com/stretchr/testify v1.12.1
)

require go.yaml.in/yaml/v3 v3.0.5 // indirect
