#!/usr/bin/env bash
# cargo for a checkout on a Windows drive under WSL: the target directory on
# a WSL-native path (builds there are slow and file locks misbehave), and, on
# a host without a system C compiler to link with, zig's (a wrapper `zcc`
# running `zig cc "$@"`, as cpp/README.md's `zcxx`).
cd "$(dirname "$0")"
export CARGO_TARGET_DIR=${CARGO_TARGET_DIR:-$HOME/clepsydra-rust-target}
if ! command -v cc >/dev/null && command -v zcc >/dev/null; then
	export CARGO_TARGET_X86_64_UNKNOWN_LINUX_GNU_LINKER=${CARGO_TARGET_X86_64_UNKNOWN_LINUX_GNU_LINKER:-zcc}
fi
exec cargo "$@"
