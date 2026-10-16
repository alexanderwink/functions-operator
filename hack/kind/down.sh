#!/usr/bin/env bash
set -euo pipefail
export KIND_EXPERIMENTAL_PROVIDER=podman
# Prefer the Homebrew kind (>= 0.20); an old kind on PATH picks an outdated node image.
export PATH="/opt/homebrew/bin:$PATH"
kind delete cluster --name functions
