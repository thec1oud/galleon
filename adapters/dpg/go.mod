module github.com/thec1oud/galleon/adapters/dpg

go 1.26.0

// dpg's library engine isn't published yet: build against a local dpg
// checkout linked at the repository root's .dpg (scripts/link-dpg.sh).
replace github.com/thec1oud/dpg => ../../.dpg/core

// dpg builds against its fork of pg_query_go, which Go doesn't take from a
// dependency's go.mod: this repeats dpg's replace, and scripts/link-dpg.sh
// checks that it still matches the linked checkout's.
replace github.com/pganalyze/pg_query_go/v6 => github.com/thec1oud/dpg_query_go/v6 v6.0.0-20260825050250-06b53111aff1

// Until the core is tagged, the adapter builds against the core and the
// testkit in this repository.
replace (
	github.com/thec1oud/galleon => ../..
	github.com/thec1oud/galleon/testkit => ../../testkit
)

require (
	github.com/jackc/pgx/v5 v5.11.0
	github.com/thec1oud/dpg v0.0.0-00010101000000-000000000000
	github.com/thec1oud/galleon v0.0.0-00010101000000-000000000000
	github.com/thec1oud/galleon/testkit v0.0.0-00010101000000-000000000000
)

require (
	github.com/BurntSushi/toml v1.6.0 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/pganalyze/pg_query_go/v6 v6.2.2 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/otel v1.47.0 // indirect
	go.opentelemetry.io/otel/log v1.47.0 // indirect
	go.opentelemetry.io/otel/metric v1.47.0 // indirect
	go.opentelemetry.io/otel/trace v1.47.0 // indirect
	golang.org/x/sync v0.21.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
	golang.org/x/text v0.38.0 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
)
