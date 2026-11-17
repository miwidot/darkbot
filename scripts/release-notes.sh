#!/usr/bin/env bash
# Prints the release notes for tag $1: every commit since the previous tag
# (title as heading, message body as text), then the downloads. Used by the
# release workflow; darkbot shows the "What's new" part in its update panel.
#
#   scripts/release-notes.sh v0.2.3
set -euo pipefail
tag="$1"
repo="${GITHUB_REPOSITORY:-miwidot/darkbot}"
prev=$(git describe --tags --abbrev=0 "$tag^" 2>/dev/null || true)
range="$tag"
[ -n "$prev" ] && range="$prev..$tag"

echo "## What's new"
found=0
# %x1e separates commits; "Release vX" commits only bump versions.
while IFS= read -r -d $'\x1e' entry; do
  entry="${entry#$'\n'}"
  subject="${entry%%$'\n'*}"
  [ -z "$subject" ] && continue
  [[ "$subject" =~ ^Release\ v[0-9] ]] && continue
  found=1
  echo
  echo "### $subject"
  body="${entry#"$subject"}"
  body=$(printf '%s\n' "$body" | { grep -v -i -E '^(co-authored-by|signed-off-by):' || true; } | sed -e '/./,$!d')
  if [ -n "$body" ]; then
    echo
    printf '%s\n' "$body"
  fi
done < <(git log --no-merges --reverse --format='%B%x1e' "$range")
if [ "$found" = 0 ]; then
  echo
  echo "Maintenance release."
fi

cat <<NOTES

## Download

- Windows 10/11: \`darkbot-windows-amd64.exe\`
- macOS, Apple Silicon: \`darkbot-macos-arm64.app.zip\`
- macOS, Intel: \`darkbot-macos-amd64.app.zip\`
- Linux: \`darkbot-linux-amd64\`

Already installed? darkbot updates itself on the next start.
NOTES
if [ -n "$prev" ]; then
  echo
  echo "Full changelog: https://github.com/$repo/compare/$prev...$tag"
fi
