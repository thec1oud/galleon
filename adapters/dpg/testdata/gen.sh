#!/bin/sh
# Writes the conformance suite's builds (migratetest.Build) as DPG projects
# whose migration logs record the suite's changes, one table each:
#
#	base    a, b
#	next    base's log, then c
#	last    next's log, then d
#	risky   base's log, then dropping a (Destructive)
#	edited  a with another column, then b: a log that diverges from base's
#	indexed base's log, then an index on a, made CONCURRENTLY after the
#	        migration's transaction
#	noted   indexed's log, then a column on a
#
# The builds that extend another copy its project and record one more
# migration, so they share its migrations' IDs, as a newer build of a
# project does. Run it from adapters/dpg with dpg on PATH (or DPG set to
# the binary), after changing the builds or upgrading DPG:
#
#	go build -o /tmp/dpg ./.dpg/core/cmd/dpg   # from the repository root
#	DPG=/tmp/dpg testdata/gen.sh
set -eu
DPG=${DPG:-dpg}
out=testdata/builds
cd "$(dirname "$0")/.."
rm -rf "$out"
mkdir -p "$out"

project() {
	mkdir -p "$out/$1/c/app/schemas/public/tables"
	printf 'kind = "root"\n' >"$out/$1/dpg.toml"
	printf 'kind = "cluster"\n\n[cluster]\nname = "c"\n' >"$out/$1/c/dpg.toml"
	printf 'kind = "database"\n\n[database]\nname = "app"\ndefault_schema = "public"\n' >"$out/$1/c/app/dpg.toml"
}

# table <build> <name> [<columns>] declares a table.
table() {
	printf 'TABLE %s (%s);\n' "$2" "${3:-id int PRIMARY KEY}" >"$out/$1/c/app/schemas/public/tables/$2.dpg"
}

record() {
	b=$1
	shift
	"$DPG" -C "$out/$b" plan --record "$@" >/dev/null
}

project base
table base a
record base
table base b
record base

cp -R "$out/base" "$out/next"
table next c
record next

cp -R "$out/next" "$out/last"
table last d
record last

cp -R "$out/base" "$out/risky"
rm "$out/risky/c/app/schemas/public/tables/a.dpg"
record risky --allow-destructive

cp -R "$out/base" "$out/indexed"
printf 'TABLE a (id int PRIMARY KEY) { INDEXES { CONCURRENTLY a_id_desc (id DESC); } }\n' >"$out/indexed/c/app/schemas/public/tables/a.dpg"
record indexed

cp -R "$out/indexed" "$out/noted"
printf 'TABLE a (id int PRIMARY KEY, note text) { INDEXES { CONCURRENTLY a_id_desc (id DESC); } }\n' >"$out/noted/c/app/schemas/public/tables/a.dpg"
record noted

project edited
table edited a "id int PRIMARY KEY, note text"
record edited
table edited b
record edited
