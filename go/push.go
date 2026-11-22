package temporalfilter

// lab/sw W13: a count of each touch's stores of its event into the drip
// log. With the tfpush tag (and tfdrip) a touch tallies, in its own frame,
// each word0 with its push that it stores (touch's CAS in the key's unit,
// stamp, realign, miss's claim or wait), each gate that kept it from pushing
// (W4c's in the table, W4d's in miss), each push next0 took without a change
// (a log that says nothing), and miss's missPlaced; at its return it classes
// itself and counts the class in the filter (push_on.go). The count is of
// stores: a push stored into a copy that is later dropped reads as stored.
// Without the tag the tally is empty and its notes do nothing (push_off.go).

// Where a touch stored its push.
const (
	pushTable   = iota // touch's CAS of word0, in the key's unit
	pushStamp          // stamp: a hot key into a later unit
	pushRealign        // realign: into a later unit
	pushClaim          // miss: installed in a slot
	pushWait           // miss: waiting for a slot (the stash or the wait room)
	pushSites
)

// The filter's push counts, pushCount.N's indices.
const (
	pcCalls      = iota                 // touches returned
	pcNone                              // classes: no store
	pcOnce                              // one store
	pcFresh                             // a later store started the key afresh (miss recalled no history)
	pcAgain                             // a later store into a history whose last touch is before the touch's tick (W4d's push-again half)
	pcTwice                             // a later store into a history whose last touch is at or after the touch's tick
	pcBehind                            // touches that stored with the library's pushed flag false after a store of theirs
	pcFirstFresh                        // touches whose first store started the key afresh
	pcBare                              // touches that returned with no store and no silent note (from the code, none)
	pcStores                            // stores by site, pushSites of them
	pcSkipTable  = iota + pushSites - 1 // (after pcStores's sites) W4c's gate held in the table
	pcSkipMiss                          // W4d's gate held in miss
	pcSilent                            // next0 left word0 as it was
	pcPlaced                            // miss returned missPlaced
	pushN
)

var pushNames = [pushN]string{"calls", "none", "once", "fresh", "again", "twice", "behind", "first fresh", "bare",
	"stores table", "stores stamp", "stores realign", "stores claim", "stores wait",
	"skips table", "skips miss", "silent", "missPlaced"}

// pushCount is a filter's push counts (pushCounts), and the first touch
// classed twice.
type pushCount struct {
	N     [pushN]int64
	First string
}

// testHookPushDone, if set, runs at each touch's return with its class and
// tally (tfpush).
var testHookPushDone func(h, now uint64, class, tally string)
