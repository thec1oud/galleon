#!/usr/bin/env bash
# Checks the architecture decision records in docs/adr (see
# docs/adr/0001-record-architecture-decisions.md).
#
# Usage: adr-check.sh [--decided] [BASE]
#
# Every file in docs/adr other than README.md and template.md must be a
# record named NNNN-slug.md, with Unix line endings, "# NNNN. Title" on line
# 1, a valid "Status:" line on line 3, a "Date:" line on line 4, and the
# sections Context, Decision, Consequences, Alternatives considered and
# Sources, in that order. Numbers must be unique. A record superseded by
# another must name a different, accepted record. The index table in
# docs/adr/README.md must have exactly one row per record, with its number,
# title, file and status, and no other rows.
#
# With --decided, a Proposed record is an error: a record is accepted or
# rejected before it merges.
#
# With BASE (a commit), it also checks that no record that was Accepted,
# Rejected, Superseded or Deprecated at BASE has been edited, renamed or
# deleted since: only its Status: line may change, and only forwards
# (Accepted to Superseded or Deprecated).
#
# Run from the repository root, with bash 4 or later (macOS's /bin/bash is
# 3.2). Exits non-zero, listing every problem, if any check fails.
set -euo pipefail

DIR=docs/adr
INDEX=$DIR/README.md
SECTIONS=("## Context" "## Decision" "## Consequences" "## Alternatives considered" "## Sources")
problems=0

fail() {
  echo "adr-check: $*" >&2
  problems=$((problems + 1))
}

decided=false
if [[ ${1:-} == --decided ]]; then
  decided=true
  shift
fi
base=${1:-}
if [[ -n $base ]] && ! git rev-parse --verify -q "$base^{commit}" >/dev/null; then
  echo "adr-check: $base is not a commit" >&2
  exit 2
fi

# status_kind prints the kind of a Status: line's value (Proposed, Accepted,
# Rejected, Deprecated or Superseded), or nothing if it isn't valid.
status_kind() {
  case $1 in
    Proposed | Accepted | Rejected | Deprecated) echo "$1" ;;
    *)
      if [[ $1 =~ ^Superseded\ by\ \[([0-9]{4})\]\(([0-9]{4})-[a-z0-9-]+\.md\)$ &&
        ${BASH_REMATCH[1]} == "${BASH_REMATCH[2]}" ]]; then
        echo Superseded
      fi
      ;;
  esac
}

# status_of prints the value of the Status: line (line 3) of the file named.
status_of() {
  sed -n '3s/^Status: //p' "$1"
}

# The records, and every file that should be one.
records=()
while IFS= read -r f; do
  name=${f#"$DIR"/}
  case $name in
    README.md | template.md) continue ;;
  esac
  if [[ ! $name =~ ^[0-9]{4}-[a-z0-9]+(-[a-z0-9]+)*\.md$ ]]; then
    fail "$f: name it NNNN-slug.md, with a lowercase, hyphenated slug, directly in $DIR"
    continue
  fi
  records+=("$f")
done < <(find "$DIR" -type f | sort)

declare -A seen=() title_of=() kind_of=() full_status_of=()
for f in "${records[@]}"; do
  name=${f##*/}
  num=${name:0:4}
  if [[ -n ${seen[$num]:-} ]]; then
    fail "$f: number $num is also used by ${seen[$num]}"
  fi
  seen[$num]=$f
  if grep -q $'\r' "$f"; then
    fail "$f: has Windows (CRLF) line endings; use Unix (LF)"
    continue
  fi

  title=$(sed -n '1p' "$f")
  if [[ $title =~ ^"# $num. "(.+)$ ]]; then
    title_of[$name]=${BASH_REMATCH[1]}
  else
    fail "$f: line 1 must be \"# $num. Title\", not \"$title\""
  fi
  status=$(status_of "$f")
  kind=$(status_kind "$status")
  kind_of[$name]=$kind
  full_status_of[$name]=$status
  if [[ -z $kind ]]; then
    fail "$f: line 3 must be \"Status: \" followed by Proposed, Accepted, Rejected, Deprecated or \"Superseded by [NNNN](NNNN-slug.md)\""
  elif [[ $kind == Proposed ]] && $decided; then
    fail "$f: still Proposed; accept or reject it before it merges"
  fi
  if ! sed -n '4p' "$f" | grep -Eq '^Date: [0-9]{4}-[0-9]{2}-[0-9]{2}$'; then
    fail "$f: line 4 must be \"Date: YYYY-MM-DD\""
  fi
  headings=$(grep -E '^## ' "$f" || true)
  want=$(printf '%s\n' "${SECTIONS[@]}")
  if [[ $headings != "$want" ]]; then
    fail "$f: its sections must be exactly: $(printf '"%s" ' "${SECTIONS[@]}")in that order"
  fi
done

# A superseded record names a different record, which is in force.
for f in "${records[@]}"; do
  name=${f##*/}
  [[ ${kind_of[$name]:-} == Superseded ]] || continue
  target=${full_status_of[$name]#*](}
  target=${target%)}
  if [[ $target == "$name" ]]; then
    fail "$f: can't be superseded by itself"
  elif [[ ! -f $DIR/$target ]]; then
    fail "$f: superseded by $target, which doesn't exist"
  elif [[ ${kind_of[$target]:-} != Accepted ]]; then
    fail "$f: superseded by $target, which isn't Accepted"
  fi
done

# The index has one row per record, and only those.
declare -A listed=()
while IFS= read -r row; do
  if [[ ! $row =~ ^\|\ ([0-9]{4})\ \|\ \[(.+)\]\(([^\)]+)\)\ \|\ (.+)\ \|$ ]]; then
    fail "$INDEX: index row not in the form \"| NNNN | [Title](NNNN-slug.md) | Status |\": $row"
    continue
  fi
  num=${BASH_REMATCH[1]} title=${BASH_REMATCH[2]} file=${BASH_REMATCH[3]} status=${BASH_REMATCH[4]}
  if [[ -n ${listed[$file]:-} ]]; then
    fail "$INDEX: $file is listed twice"
    continue
  fi
  listed[$file]=1
  if [[ ! -f $DIR/$file || -z ${seen[$num]:-} || ${seen[$num]} != "$DIR/$file" ]]; then
    fail "$INDEX: row $num points at $file, which isn't record $num"
    continue
  fi
  if [[ $title != "${title_of[$file]:-}" ]]; then
    fail "$INDEX: row $num's title doesn't match line 1 of $file"
  fi
  if [[ $status != "${full_status_of[$file]:-}" ]]; then
    fail "$INDEX: row $num's status \"$status\" doesn't match $file's \"${full_status_of[$file]:-}\""
  fi
done < <(grep -E '^\| ?[0-9]' "$INDEX" || true)
for f in "${records[@]}"; do
  name=${f##*/}
  if [[ -z ${listed[$name]:-} ]]; then
    fail "$f: not listed in the index in $INDEX"
  fi
done

# Accepted, rejected, superseded and deprecated records are append-only.
if [[ -n $base ]]; then
  base_files=$(git ls-tree -r --name-only "$base" -- "$DIR/")
  while IFS= read -r f; do
    [[ -n $f ]] || continue
    name=${f##*/}
    [[ $f == "$DIR/$name" && $name =~ ^[0-9]{4}-.*\.md$ ]] || continue
    old_status=$(git show "$base:$f" | sed -n '3s/^Status: //p')
    old_kind=$(status_kind "$old_status")
    if [[ -z $old_kind ]]; then
      echo "adr-check: warning: $f has no valid status at $base, so it isn't checked for edits" >&2
      continue
    fi
    [[ $old_kind != Proposed ]] || continue
    if [[ ! -f $f ]]; then
      fail "$f: was $old_kind at $base and has been deleted or renamed; records are append-only"
      continue
    fi
    if ! cmp -s <(git show "$base:$f" | sed 3d) <(sed 3d "$f"); then
      fail "$f: was $old_kind at $base and has been edited; only its Status: line may change. Supersede it with a new record instead"
    fi
    new_status=$(status_of "$f")
    if [[ $new_status != "$old_status" ]]; then
      case $old_kind:$(status_kind "$new_status") in
        Accepted:Superseded | Accepted:Deprecated) ;;
        *) fail "$f: status can't change from \"$old_status\" to \"$new_status\"" ;;
      esac
    fi
  done <<<"$base_files"
fi

if ((problems > 0)); then
  echo "adr-check: $problems problem(s)" >&2
  exit 1
fi
echo "adr-check: ${#records[@]} record(s) OK"
