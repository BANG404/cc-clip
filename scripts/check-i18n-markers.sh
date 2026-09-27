#!/usr/bin/env bash
# Fails when a translated file is behind its English source.
#
# A translated file starts with:
#   <!-- i18n-source: <source path> @ <full commit sha> -->
# The file is current when <source path> has no commit after <sha> on HEAD.
# Updating a translation in the same PR as its source works because the
# marker can name the source-changing commit (PRs merge with --merge, so
# that SHA survives). See docs/i18n/TRANSLATION_BRIEF.md.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

marker_re='^<!-- i18n-source: ([^[:space:]]+) @ ([^[:space:]]+) -->$'
status=0

while IFS= read -r file; do
	first=$(head -n 1 -- "$file")
	case "$first" in
	*i18n-source:*) ;;
	*) continue ;;
	esac

	if ! [[ $first =~ $marker_re ]]; then
		echo "FAIL: $file: malformed marker, expected '<!-- i18n-source: <path> @ <sha> -->': $first"
		status=1
		continue
	fi
	src=${BASH_REMATCH[1]}
	sha=${BASH_REMATCH[2]}

	if ! [[ $sha =~ ^[0-9a-f]{40}$ ]]; then
		echo "FAIL: $file: marker SHA '$sha' is not a full 40-character commit SHA (use: git log -1 --format=%H -- $src)"
		status=1
		continue
	fi
	if ! git cat-file -e "$sha^{commit}" 2>/dev/null || ! git merge-base --is-ancestor "$sha" HEAD; then
		echo "FAIL: $file: marker SHA $sha is not in the history of HEAD (a shallow clone needs fetch-depth: 0)"
		status=1
		continue
	fi
	if ! git ls-files --error-unmatch -- "$src" >/dev/null 2>&1; then
		echo "FAIL: $file: source $src is not a tracked file"
		status=1
		continue
	fi

	newer=$(git log --format='  %h %s' "$sha..HEAD" -- "$src")
	if [ -n "$newer" ]; then
		echo "FAIL: $file is behind $src: its marker is $sha, but $src changed since:"
		echo "$newer"
		echo "  Propagate those changes into $file, then set its marker to: $(git log -1 --format=%H -- "$src")"
		status=1
	else
		echo "ok: $file is current with $src @ ${sha:0:7}"
	fi
done < <(git ls-files -- '*.md')

exit "$status"
