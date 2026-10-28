//go:build !amd64

package temporalfilter

// hasCAS2: realign holds the slot (arm64's CASP would do; not yet measured).
const hasCAS2 = false

func cas2(p *uint64, old0, old1, new0, new1 uint64) bool { panic("unreachable") }
