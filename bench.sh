#!/usr/bin/env bash
# Scorecard: tests (race), then accuracy + speed, appended to results/history.md.
# Usage: ./bench.sh "what changed"
set -euo pipefail
cd "$(dirname "$0")"
go vet ./...
go test -race -short -count=1 ./... >/dev/null
GOMAXPROCS=${GOMAXPROCS:-4} go run ./cmd/tfbench -label "${1:?label required}"
