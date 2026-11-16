//go:build !tfdrip

package temporalfilter

// Without the build tag tfdrip there is no drip log: word0's last touch
// keeps its 46 bits.
const (
	dripOn    = false
	tsBits    = 46
	dripShift = tsBits
	dripBits  = 0
	dripMask  = 0
	dripV     = 0
	dripWord  = 0
)
