// The docs site's Go code: the samples the docs show, compiled and tested
// in CI like any other module. Being its own module also keeps the site out
// of the root module, so `go get github.com/thec1oud/galleon` never
// downloads it.
module github.com/thec1oud/galleon/site

go 1.26.0

ignore ./node_modules

require (
	github.com/jackc/pgx/v5 v5.11.0
	github.com/thec1oud/galleon v0.0.0-00010101000000-000000000000
	github.com/thec1oud/galleon/coord-pg v0.0.0-00010101000000-000000000000
	github.com/thec1oud/galleon/testkit v0.0.0-00010101000000-000000000000
	golang.org/x/tools v0.36.0
)

require (
	connectrpc.com/connect v1.21.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/otel v1.47.0 // indirect
	go.opentelemetry.io/otel/log v1.47.0 // indirect
	go.opentelemetry.io/otel/metric v1.47.0 // indirect
	go.opentelemetry.io/otel/trace v1.47.0 // indirect
	golang.org/x/mod v0.27.0 // indirect
	golang.org/x/sync v0.17.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
	golang.org/x/text v0.29.0 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
)

replace (
	github.com/thec1oud/galleon => ../
	github.com/thec1oud/galleon/coord-pg => ../coord-pg
	github.com/thec1oud/galleon/testkit => ../testkit
)
