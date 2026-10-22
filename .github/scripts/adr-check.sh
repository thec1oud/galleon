#!/usr/bin/env bash
# Checks the architecture decision records in docs/adr (see
# docs/adr/0001-record-architecture-decisions.md).
#
# Usage: adr-check.sh [BASE]
#
# Every record must be named NNNN-slug.md, start with "# NNNN. Title", carry
# a valid "Status:" line and a "Date:" line, and be listed in the index
# (docs/adr/README.md). Numbers must be unique.
#
# With BASE (a commit, usually the merge base of a pull request), it also
# checks that no record that was Accepted, Rejected, Superseded or Deprecated
# at BASE has been edited, renamed or deleted since: only its Status: line
# may change, and only forwards (Accepted to Superseded or Deprecated).
#
# Run from the repository root. Exits non-zero, listing every problem, if
# any check fails.
set -euo pipefail

DIR=docs/adr
INDEX=$DIR/README.md
problems=0

fail() {
  echo "adr-check: $*" >&2
  problems=$((problems + 1))
}

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

# status_of prints the value of the Status: line (line 3) in the text on stdin.
status_of() {
  sed -n '3s/^Status: //p'
}

# without_status prints the text on stdin without its Status: line.
without_status() {
  sed '3d'
}

records=()
while IFS= read -r f; do
  records+=("$f")
done < <(find "$DIR" -maxdepth 1 -name '[0-9]*.md' | sort)

declare -A seen=()
for f in "${records[@]}"; do
  name=${f##*/}
  if [[ ! $name =~ ^([0-9]{4})-[a-z0-9]+(-[a-z0-9]+)*\.md$ ]]; then
    fail "$f: name it NNNN-slug.md, with a lowercase, hyphenated slug"
    continue
  fi
  num=${BASH_REMATCH[1]}
  if [[ -n ${seen[$num]:-} ]]; then
    fail "$f: number $num is also used by ${seen[$num]}"
  fi
  seen[$num]=$f

  title=$(sed -n '1p' "$f")
  if [[ ! $title =~ ^"# $num. ". ]]; then
    fail "$f: line 1 must be \"# $num. Title\", not \"$title\""
  fi
  status=$(status_of <"$f")
  kind=$(status_kind "$status")
  if [[ -z $kind ]]; then
    fail "$f: line 3 must be \"Status: \" followed by Proposed, Accepted, Rejected, Deprecated or \"Superseded by [NNNN](NNNN-slug.md)\""
  elif [[ $kind == Superseded ]]; then
    target=${status#*](}
    target=${target%)}
    if [[ ! -f $DIR/$target ]]; then
      fail "$f: superseded by $target, which doesn't exist"
    fi
  fi
  if ! sed -n '4p' "$f" | grep -Eq '^Date: [0-9]{4}-[0-9]{2}-[0-9]{2}$'; then
    fail "$f: line 4 must be \"Date: YYYY-MM-DD\""
  fi
  if ! grep -Fq "]($name)" "$INDEX"; then
    fail "$f: not listed in $INDEX"
  fi
done

if [[ $# -ge 1 && -n $1 ]]; then
  base=$1
  while IFS= read -r f; do
    name=${f##*/}
    [[ $name =~ ^[0-9]{4}-.*\.md$ ]] || continue
    old=$(git show "$base:$f")
    old_kind=$(status_kind "$(status_of <<<"$old")")
    case $old_kind in
      Proposed | "") continue ;;
    esac
    if [[ ! -f $f ]]; then
      fail "$f: was $old_kind at $base and has been deleted or renamed; records are append-only"
      continue
    fi
    if [[ "$(without_status <<<"$old")" != "$(without_status <"$f")" ]]; then
      fail "$f: was $old_kind at $base and has been edited; only its Status: line may change. Supersede it with a new record instead"
    fi
    new_status=$(status_of <"$f")
    new_kind=$(status_kind "$new_status")
    old_status=$(status_of <<<"$old")
    if [[ $new_status != "$old_status" ]]; then
      case $old_kind:$new_kind in
        Accepted:Superseded | Accepted:Deprecated) ;;
        *) fail "$f: status can't change from \"$old_status\" to \"$new_status\"" ;;
      esac
    fi
  done < <(git ls-tree -r --name-only "$base" -- "$DIR/")
fi

if ((problems > 0)); then
  echo "adr-check: $problems problem(s)" >&2
  exit 1
fi
echo "adr-check: ${#records[@]} record(s) OK"
