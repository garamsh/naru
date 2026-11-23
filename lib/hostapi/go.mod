module github.com/garamsh/naru/lib/hostapi

go 1.26.0

require github.com/stretchr/testify v1.12.1

require (
	github.com/garamsh/naru/lib/wire v0.0.0-00010101000000-000000000000
	go.yaml.in/yaml/v3 v3.0.5 // indirect
)

replace github.com/garamsh/naru/lib/wire => ../wire
