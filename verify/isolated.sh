#!/usr/bin/env bash
# Runs verify/run.sh on a fresh copy of this repository's committed history,
# with no network, empty Go caches and nothing from this machine's Go setup.
#
#   verify/isolated.sh [--speed]          Docker if available, else unshare
#   ISOLATION=unshare verify/isolated.sh  force the no-Docker path
#
# Docker path: the official Go image pinned by digest, --network none, the
# repository passed in as a read-only git bundle. The image is the only
# input besides the bundle; pull it once beforehand (it is the multi-arch
# golang:1.24.7-bookworm index, also served as
# mirror.gcr.io/library/golang:1.24.7-bookworm).
#
# unshare path: needs Go 1.24.7 on PATH and unprivileged user namespaces.
# The run gets a new network namespace (loopback only), env -i, and fresh
# HOME, GOCACHE, GOPATH and GOMODCACHE under a temp dir.
#
# Only committed work is verified; uncommitted changes are not in the bundle.
# The report lands in ./verify-report-isolated (override with OUT=...).
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
IMAGE=${IMAGE:-golang:1.24.7-bookworm@sha256:b8bae5bd9ba9b1f89b635c91c24cc75cea335a16fb5076310f38566fc674b1ec}
OUT=${OUT:-$PWD/verify-report-isolated}
rm -rf "$OUT" && mkdir -p "$OUT"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
if [ -n "$(git status --porcelain --untracked-files=no)" ]; then
	echo "note: uncommitted changes are not verified" >&2
fi
git bundle create -q "$tmp/repo.bundle" --all
# Go settings for both paths: never download modules or toolchains.
goenv=(GOFLAGS=-mod=readonly GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local GOTELEMETRY=off)

mode=${ISOLATION:-}
if [ -z "$mode" ]; then
	if docker info >/dev/null 2>&1; then mode=docker; else mode=unshare; fi
fi
echo "isolation: $mode" | tee "$OUT/isolation.txt"
case $mode in
docker)
	docker image inspect "$IMAGE" >/dev/null 2>&1 || {
		echo "image $IMAGE not present locally; pull it first (docker pull)" >&2
		exit 2
	}
	echo "image: $IMAGE" >>"$OUT/isolation.txt"
	docker run --rm --network none \
		-v "$tmp/repo.bundle:/in/repo.bundle:ro" -v "$OUT:/out" \
		"${goenv[@]/#/-e}" -e OUT=/out -e WORK=/tmp/work \
		"$IMAGE" bash -c 'git clone -q /in/repo.bundle /src && cd /src && verify/run.sh "$@"' _ "$@"
	;;
unshare)
	gobin=$(dirname "$(command -v go)")
	git clone -q "$tmp/repo.bundle" "$tmp/src"
	mkdir -p "$tmp/home" "$tmp/work"
	(cd "$tmp/src" && env -i PATH="$gobin:/usr/bin:/bin" HOME="$tmp/home" \
		GOCACHE="$tmp/gocache" GOPATH="$tmp/gopath" GOMODCACHE="$tmp/gomodcache" \
		"${goenv[@]}" OUT="$OUT" WORK="$tmp/work" \
		unshare -rn verify/run.sh "$@")
	;;
*)
	echo "unknown ISOLATION=$mode" >&2
	exit 2
	;;
esac
