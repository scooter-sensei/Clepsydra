#include "textflag.h"

// func prefetchLines(words unsafe.Pointer, lines *uint64, n int)
TEXT ·prefetchLines(SB),NOSPLIT,$0-24
	MOVD words+0(FP), R0
	MOVD lines+8(FP), R1
	MOVD n+16(FP), R2
	CMP $0, R2
	BLE done
loop:
	MOVD.P 8(R1), R3
	LSL $6, R3, R3
	ADD R0, R3, R3
	PRFM (R3), PLDL1KEEP
	SUBS $1, R2, R2
	BNE loop
done:
	RET
