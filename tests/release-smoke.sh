#!/bin/sh
set -eu
# The release workflow must build the Windows arm64 target. The build is a
# target-matrix loop (for target in ... windows/arm64 ...), so assert the
# matrix entry rather than a now-stale literal step name.
grep -q "windows/arm64" .github/workflows/release.yml
if git rev-list --objects --all | grep -E '\.(exe|dll|so|dylib|a|o|db|sqlite|zip)$'; then
  echo "binary or data artifact found in Cortex history" >&2
  exit 1
fi
echo "release smoke: ok"
