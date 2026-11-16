//go:build tfdrip

package temporalfilter

// lab/sw-2026-11, arm W4: the drip log (see drip.go). With the build tag
// tfdrip, word0's last touch keeps 34 bits (MaxTick 2^34-1: 4.8 hours at
// 1 µs, 199 days at 1 ms) and its other 12 hold the drip log (11 bits) and its valid bit (W4c).
const (
	dripOn    = true
	tsBits    = 34
	dripShift = tsBits
	dripBits  = 11
	dripMask  = 1<<dripBits - 1
	dripV     = 1 << dripBits // the log's valid bit, above its field (W4c)
	dripWord  = (dripMask | dripV) << dripShift
)
