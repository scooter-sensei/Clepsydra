# Shared by the verify scripts. The Go module lives in go/ since the
# repository became multi-language; older commits have it at the root.
TOP=$(git rev-parse --show-toplevel)

# export_tree <rev> <dir>: a commit's Go module files (no .git) in <dir>.
export_tree() {
	mkdir -p "$2" && git -C "$TOP" archive "$1" | tar -x -C "$2"
	if [ -f "$2/go/go.mod" ]; then
		mv "$2" "$2.full" && mv "$2.full/go" "$2" && rm -rf "$2.full"
	fi
}
