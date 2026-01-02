#include "textflag.h"

// func Execute(codePtr, stackCursor, memoryPtr, memoryLen, inputPtr, inputLen)
TEXT ·Execute(SB), NOSPLIT, $0-48
    PUSHQ BP
    MOVQ SP, BP

    MOVQ codePtr+0(FP), R10
    MOVQ stackCursor+8(FP), DI
    MOVQ memoryPtr+16(FP), SI
    MOVQ memoryLen+24(FP), DX
    MOVQ inputPtr+32(FP), CX
    MOVQ inputLen+40(FP), R8

	// Subtract 512 bytes to ensure:
	// 1. Red Zone (128 bytes) compliance (System V ABI)
	// 2. Alignment padding (up to 16 bytes)
	// 3. Safety buffer for OS signal stack frames (~300 bytes depending on arch)
    SUBQ $512, SP
    ANDQ $-16, SP

    CALL R10

    MOVQ BP, SP
    POPQ BP
    RET
