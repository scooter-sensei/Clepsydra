package temporalfilter

// Scratch diagnostic (W10, case 865): addSketch reports the overflow
// table's part it adds. Never in the tree.
var testHookW10Sketch func(h uint64, s Result, ok bool)
