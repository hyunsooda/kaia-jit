package jitcall

import "unsafe"

func Execute(
	codePtr unsafe.Pointer,
	stackCursor unsafe.Pointer,
	memoryPtr unsafe.Pointer,
	inputPtr unsafe.Pointer,
	inputLen uint64,
)
