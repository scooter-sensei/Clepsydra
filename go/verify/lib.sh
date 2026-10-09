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

# fix_imports <dir>: after copying today's files into an older tree, point
# their imports at that tree's module path (it changed with the move).
fix_imports() {
	local m
	m=$(awk '/^module /{print $2}' "$1/go.mod")
	grep -rl --include='*.go' 'github.com/scooter-sensei/clepsydra/go' "$1" |
		xargs -r sed -i "s#github.com/scooter-sensei/clepsydra/go#$m#g"
}
