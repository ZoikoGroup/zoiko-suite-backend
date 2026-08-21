#!/usr/bin/env bash
# Keep every migration's line-1 header comment equal to its own filename, and
# renumber the whole set when files are added, removed or reordered.
#
#   ./sync-migrations.sh              check only — report drift, change nothing
#   ./sync-migrations.sh --fix        rewrite each line-1 header to match its file
#   ./sync-migrations.sh --renumber   renumber to 0001..NNNN in order, then --fix
#
# THE FILENAME IS THE SOURCE OF TRUTH, not the header. The filename is what
# decides apply order, what git tracks, and what a person reads first — so the
# comment inside follows it, never the other way round. A header that disagrees
# with its filename is worse than no header: it is a confident wrong answer to
# "which migration am I looking at".
#
# --renumber exists because inserting a migration in the middle otherwise means
# renaming every file after it by hand and editing every header to match.

set -euo pipefail
export MSYS_NO_PATHCONV=1

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DIR="$HERE/migrations"
MODE="${1:-check}"

case "$MODE" in
    check|--check) MODE=check ;;
    --fix)         MODE=fix ;;
    --renumber)    MODE=renumber ;;
    *) echo "usage: $(basename "$0") [--fix|--renumber]"; exit 2 ;;
esac

shopt -s nullglob
files=("$DIR"/*.sql)
[ ${#files[@]} -gt 0 ] || { echo "no migrations in $DIR"; exit 1; }

# Use git mv inside a work tree so renames stay renames in history rather than
# showing up as a delete plus an unrelated add.
if git -C "$HERE" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
    MV=(git -C "$HERE" mv)
else
    MV=(mv)
fi

# ── renumber ────────────────────────────────────────────────────────────────
if [ "$MODE" = renumber ]; then
    # Two phases. Going straight to the final names can collide part-way
    # through — renaming 0002 to 0001 while a 0001 still exists fails, and on a
    # case-insensitive filesystem it can silently clobber instead. Moving
    # everything aside first makes the operation order-independent.
    echo "renumbering ${#files[@]} migrations"
    tmp_names=()
    i=0
    for f in "${files[@]}"; do
        t="$DIR/.renumber.$i.tmp"
        mv "$f" "$t"
        tmp_names+=("$t")
        i=$((i+1))
    done

    i=1
    for t in "${tmp_names[@]}"; do
        # Recover the descriptive half: everything after the first underscore.
        # A file with no numeric prefix keeps its whole name as the description.
        orig=$(basename "${files[$((i-1))]}")
        desc="${orig#*_}"
        [ "$desc" = "$orig" ] && desc="$orig"
        new=$(printf "%04d_%s" "$i" "$desc")
        mv "$t" "$DIR/$new"
        # Re-register with git so the move is recorded, not just done on disk.
        if [ "${MV[0]}" = git ]; then
            git -C "$HERE" add -A "migrations/$new" >/dev/null 2>&1 || true
        fi
        [ "$orig" = "$new" ] || printf "  %-46s -> %s\n" "$orig" "$new"
        i=$((i+1))
    done
    if [ "${MV[0]}" = git ]; then
        git -C "$HERE" add -A migrations >/dev/null 2>&1 || true
    fi
    files=("$DIR"/*.sql)
    MODE=fix
fi

# ── header sync ─────────────────────────────────────────────────────────────
drift=0
for f in "${files[@]}"; do
    base=$(basename "$f")
    first=$(head -1 "$f")
    want="-- $base"

    [ "$first" = "$want" ] && continue

    # Replace line 1 ONLY when it is itself a filename header — a comment whose
    # entire content is something ending in .sql. Anything else gets the header
    # inserted ABOVE it.
    #
    # "is it a comment" is not a safe enough test. A file whose header line was
    # deleted has some other comment on line 1 — a description, a section rule —
    # and treating that as the header to overwrite silently destroys it. Line 1
    # being a comment says nothing about whether it is THIS comment.
    if [[ "$first" =~ ^--[[:space:]]+[A-Za-z0-9_.-]+\.sql[[:space:]]*$ ]]; then
        action=replace
    else
        action=insert
    fi

    if [ "$MODE" = check ]; then
        drift=$((drift+1))
        printf "  DRIFT  %-46s line 1 says: %s\n" "$base" "${first:0:60}"
        continue
    fi

    if [ "$action" = replace ]; then
        # In-place, first line only. Nothing else in the file is touched.
        sed -i "1s|^-- .*|$want|" "$f"
        printf "  fixed  %-46s\n" "$base"
    else
        printf '%s\n' "$want" | cat - "$f" > "$f.tmp" && mv "$f.tmp" "$f"
        printf "  added  %-46s (header inserted; nothing overwritten)\n" "$base"
    fi
done

if [ "$MODE" = check ]; then
    if [ "$drift" -eq 0 ]; then
        echo "  all ${#files[@]} headers match their filenames"
    else
        echo
        echo "$drift file(s) out of sync — run: $(basename "$0") --fix"
        exit 1
    fi
    exit 0
fi

# The combined file embeds each filename in a FILE: banner, so it is stale the
# moment anything is renamed.
if [ -x "$HERE/build-combined.sh" ]; then
    echo
    "$HERE/build-combined.sh"
fi

echo
echo "Now re-run ./verify.sh before committing — renumbering changes apply order."
