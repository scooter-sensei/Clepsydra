#include "textflag.h"

// func rdtscp() uint64
TEXT ·rdtscp(SB), NOSPLIT, $0-8
	RDTSCP
	SHLQ $32, DX
	ORQ  DX, AX
	MOVQ AX, ret+0(FP)
	RET
