package jitcall

import "unsafe"

// [변경]
func Execute(
	codePtr unsafe.Pointer,
	stackCursor unsafe.Pointer,
	inputPtr unsafe.Pointer,
	inputLen uint64,
)
