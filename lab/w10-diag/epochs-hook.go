package temporalfilter

// Scratch diagnostic (W10, case 865): addSketch reports the overflow
// table's part it adds; ghost.estimate reports the epoch, until and bucket
// words it decoded and its answer. Never in the tree.
var testHookW10Sketch func(h uint64, s Result, ok bool)
var testHookW10Est func(id, now, ep, until uint64, line [2][ghWords]uint64, nb int, r Result)
