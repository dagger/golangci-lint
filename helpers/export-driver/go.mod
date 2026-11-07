module github.com/dagger/golangci-lint/helpers/export-driver

go 1.26.0

require (
	github.com/dagger/golangci-lint/helpers/export-driver/manifest v0.0.0
	golang.org/x/tools v0.50.0
)

require (
	golang.org/x/mod v0.41.0 // indirect
	golang.org/x/sync v0.23.0 // indirect
)

replace github.com/dagger/golangci-lint/helpers/export-driver/manifest => ./manifest
