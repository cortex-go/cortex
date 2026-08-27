#!/bin/sh
set -eu
grep -q "build windows arm64" .github/workflows/release.yml
if git rev-list --objects --all | grep -E '\.(exe|dll|so|dylib|a|o|db|sqlite|zip)$'; then
  echo "binary or data artifact found in Cortex history" >&2
  exit 1
fi
echo "release smoke: ok"
