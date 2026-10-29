#include "textflag.h"

// func CAS2(p *uint64, old0, old1, new0, new1 uint64) bool
TEXT ·CAS2(SB), NOSPLIT, $0-41
	MOVQ p+0(FP), DI
	MOVQ old0+8(FP), AX
	MOVQ old1+16(FP), DX
	MOVQ new0+24(FP), BX
	MOVQ new1+32(FP), CX
	LOCK
	CMPXCHG16B (DI)
	SETEQ ret+40(FP)
	RET

// func cpuHasCX16() bool
TEXT ·cpuHasCX16(SB), NOSPLIT, $0-1
	MOVL $1, AX
	XORL CX, CX
	CPUID
	SHRL $13, CX
	ANDL $1, CX
	MOVB CX, ret+0(FP)
	RET
