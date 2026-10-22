#!/usr/bin/env bash
# Tests adr-check.sh against scratch repositories: each case builds a base
# commit, changes it, and checks whether adr-check passes or fails.
set -euo pipefail

CHECK=$(cd "$(dirname "$0")" && pwd)/adr-check.sh
failures=0

# record NUM SLUG STATUS [BODY] writes docs/adr/NUM-SLUG.md.
record() {
  {
    printf '# %s. Title of %s\n\nStatus: %s\nDate: 2026-10-22\n\n' "$1" "$2" "$3"
    printf '## Context\n\n%s\n\n' "${4:-Some context.}"
    printf '## Decision\n\nX.\n\n## Consequences\n\nY.\n\n## Alternatives considered\n\nZ.\n\n## Sources\n\nNone.\n'
  } >"docs/adr/$1-$2.md"
}

# index writes the index table in docs/adr/README.md from the records.
index() {
  {
    printf '# Index\n\n| # | Decision | Status |\n|---|---|---|\n'
    for f in docs/adr/[0-9]*.md; do
      local name=${f##*/}
      printf '| %s | [%s](%s) | %s |\n' "${name:0:4}" "$(sed -n '1s/^# [0-9]*\. //p' "$f")" "$name" "$(sed -n '3s/^Status: //p' "$f")"
    done
  } >docs/adr/README.md
}

# setup starts a scratch repository with 0001 Accepted and 0002 Proposed,
# committed as the base.
setup() {
  dir=$(mktemp -d)
  cd "$dir"
  git init -q
  git config user.email test@example.com
  git config user.name test
  mkdir -p docs/adr
  record 0001 first Accepted
  record 0002 second Proposed
  index
  git add -A
  git commit -qm base
}

# expect pass NAME, or expect fail NAME TEXT, runs adr-check (with ARGS, by
# default the base commit). A failure must report TEXT, so a case can't pass
# by failing for another reason.
expect() {
  local want=$1 name=$2 text=${3:-} got out
  local args=("${ARGS[@]:-$(git rev-list --max-parents=0 HEAD)}")
  if out=$("$CHECK" "${args[@]}" 2>&1); then got=pass; else got=fail; fi
  ARGS=()
  if [[ $got != "$want" ]]; then
    echo "FAIL: $name: want $want, got $got" >&2
    echo "$out" >&2
    failures=$((failures + 1))
  elif [[ $want == fail && $out != *"$text"* ]]; then
    echo "FAIL: $name: output doesn't mention \"$text\"" >&2
    echo "$out" >&2
    failures=$((failures + 1))
  else
    echo "ok: $name"
  fi
  cd / && rm -rf "$dir"
}
ARGS=()

setup
expect pass "no change"

setup
record 0002 second Accepted "Rewritten while proposed."
index
expect pass "edit a proposed record and accept it"

setup
record 0003 third Accepted
index
expect pass "add a record"

setup
record 0003 third Accepted
expect fail "add a record missing from the index" "not listed in"

setup
record 0001 first Accepted "Changed context."
expect fail "edit an accepted record" "has been edited"

setup
record 0003 third Accepted
index
record 0001 first "Superseded by [0003](0003-third.md)"
index
expect pass "supersede an accepted record"

setup
record 0001 first "Superseded by [0009](0009-missing.md)"
index
expect fail "supersede with a record that doesn't exist" "which doesn't exist"

setup
record 0003 third Accepted
index
record 0001 first "Superseded by [0002](0003-third.md)"
index
expect fail "supersede with a mismatched link" "line 3 must be"

setup
record 0001 first Deprecated
index
expect pass "deprecate an accepted record"

setup
record 0001 first Proposed
index
expect fail "reopen an accepted record" "status can't change"

setup
rm docs/adr/0001-first.md
index
expect fail "delete an accepted record" "deleted or renamed"

setup
git mv docs/adr/0001-first.md docs/adr/0001-renamed.md
index
expect fail "rename an accepted record" "deleted or renamed"

setup
record 0002 other Proposed
index
expect fail "reuse a number" "is also used by"

setup
record 0003 third Approved
index
expect fail "unknown status" "line 3 must be"

setup
sed -i '1s/.*/# 0009. Wrong number/' docs/adr/0002-second.md
expect fail "title number doesn't match the file name" "line 1 must be"

setup
sed -i '4d' docs/adr/0002-second.md
expect fail "missing date" "line 4 must be"

setup
printf '# 0003. Title\n\nStatus: Accepted\nDate: 2026-10-22\n' >docs/adr/0003_Bad_Name.md
index
expect fail "badly named file" "name it NNNN-slug.md"

setup
record 0001 first "Superseded by [0001](0001-first.md)"
index
expect fail "supersede a record with itself" "by itself"

setup
record 0001 first "Superseded by [0002](0002-second.md)"
index
expect fail "supersede with a proposed record" "isn't Accepted"

setup
record 0001 first Deprecated
expect fail "index status out of date" "doesn't match"

setup
echo '| 0009 | [Ghost](0009-ghost.md) | Accepted |' >>docs/adr/README.md
expect fail "index row for a record that doesn't exist" "isn't record 0009"

setup
sed -i 's/Title of first/Other title/' docs/adr/README.md
expect fail "index title out of date" "title doesn't match"

setup
printf '# 0023. X\n' >docs/adr/adr-0023.md
expect fail "misnamed record" "name it NNNN-slug.md"

setup
mkdir docs/adr/old
cp docs/adr/0001-first.md docs/adr/old/0005-x.md
expect fail "record in a subdirectory" "directly in docs/adr"

setup
printf '\n\n' >>docs/adr/0001-first.md
expect fail "add trailing blank lines to an accepted record" "has been edited"

setup
sed -i 's/$/\r/' docs/adr/0002-second.md
expect fail "CRLF line endings" "CRLF"

setup
sed -i '/^## Sources/,$d' docs/adr/0002-second.md
expect fail "missing section" "its sections must be"

setup
ARGS=(--decided)
expect fail "a proposed record with --decided" "still Proposed"

setup
record 0002 second Accepted
index
ARGS=(--decided)
expect pass "no proposed records with --decided"

setup
ARGS=(deadbeef)
expect fail "a base that isn't a commit" "is not a commit"

if ((failures > 0)); then
  echo "$failures case(s) failed" >&2
  exit 1
fi
