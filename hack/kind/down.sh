#!/usr/bin/env bash
set -euo pipefail
export KIND_EXPERIMENTAL_PROVIDER=podman
KIND="${KIND:-kind}"
# Require kind >= 0.20 (older versions pick an outdated node image). Override with KIND=/path/to/kind.
kind_ver="$("$KIND" version 2>/dev/null | sed -nE 's/^kind v([0-9]+)\.([0-9]+).*/\1 \2/p')" || true
read -r kmaj kmin <<<"${kind_ver:-0 0}"
if [ "$kmaj" -eq 0 ] && [ "$kmin" -lt 20 ]; then
  echo "error: '$KIND' is kind ${kmaj}.${kmin}; need >= 0.20. Set KIND=/path/to/newer/kind." >&2
  exit 1
fi
"$KIND" delete cluster --name functions
