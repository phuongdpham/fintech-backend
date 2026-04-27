module github.com/phuongdpham/fintech/apps/compliance-cli

go 1.26.2

// Internal monorepo modules. go.work makes them resolvable for
// `go build` / `go test`, but `go mod tidy` runs per-module and
// ignores the workspace — without the replace below it tries to
// fetch from the network and fails.
require github.com/phuongdpham/fintech/libs/go/logger v0.0.0

replace github.com/phuongdpham/fintech/libs/go/logger => ../../libs/go/logger
