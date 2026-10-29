#include "textflag.h"

// func PrefetchLines(words unsafe.Pointer, lines *uint64, n int)
TEXT ·PrefetchLines(SB),NOSPLIT,$0-24
	MOVQ words+0(FP), AX
	MOVQ lines+8(FP), BX
	MOVQ n+16(FP), CX
	TESTQ CX, CX
	JLE done
loop:
	MOVQ (BX), DX
	SHLQ $6, DX
	PREFETCHT0 (AX)(DX*1)
	ADDQ $8, BX
	DECQ CX
	JNZ loop
done:
	RET
