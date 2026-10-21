#!/bin/sh
# Links a local dpg checkout at the repository root's .dpg, where
# adapters/dpg/go.mod's replace directive finds dpg's module (.dpg/core).
# The checkout must have dpg's library engine (core/pkg/dpg/engine), which
# isn't published yet.
#
#	adapters/dpg/scripts/link-dpg.sh ../dpg
#
# It replaces an earlier link, but never a .dpg that isn't one. It also
# checks that the adapter's replace of pg_query_go is the one the checkout
# builds with: Go takes replace directives only from the main module.
set -eu
if [ $# -ne 1 ]; then
	echo "usage: $0 <path to a dpg checkout>" >&2
	exit 2
fi
adapter=$(cd "$(dirname "$0")/.." && pwd)
root=$(cd "$adapter/../.." && pwd)
src=$(cd "$1" && pwd)

if ! grep -qx 'module github.com/thec1oud/dpg' "$src/core/go.mod" 2>/dev/null; then
	echo "$src isn't a dpg checkout: $src/core/go.mod isn't github.com/thec1oud/dpg's" >&2
	exit 1
fi
if [ ! -d "$src/core/pkg/dpg/engine" ]; then
	echo "$src has no library engine (core/pkg/dpg/engine): check out a dpg branch that has it" >&2
	exit 1
fi
want=$(grep '^replace github.com/pganalyze/pg_query_go/v6 ' "$src/core/go.mod" || true)
have=$(grep '^replace github.com/pganalyze/pg_query_go/v6 ' "$adapter/go.mod" || true)
if [ "$want" != "$have" ]; then
	echo "adapters/dpg/go.mod's pg_query_go replace doesn't match the checkout's:" >&2
	echo "  checkout: ${want:-none}" >&2
	echo "  adapter:  ${have:-none}" >&2
	exit 1
fi

link=$root/.dpg
if [ -L "$link" ]; then
	rm "$link"
elif [ -e "$link" ]; then
	echo "$link exists and isn't a link; move it away first" >&2
	exit 1
fi
ln -s "$src" "$link"
echo "linked $link -> $src"
