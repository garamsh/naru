module github.com/garamsh/naru/naructl

go 1.26.0

require (
	github.com/garamsh/naru/lib/identity v0.0.0-00010101000000-000000000000
	github.com/garamsh/naru/lib/orchapi v0.0.0-00010101000000-000000000000
	github.com/stretchr/testify v1.12.1
	go.yaml.in/yaml/v3 v3.0.5
)

require golang.org/x/crypto v0.45.0 // indirect

replace github.com/garamsh/naru/lib/identity => ../lib/identity

replace github.com/garamsh/naru/lib/orchapi => ../lib/orchapi
