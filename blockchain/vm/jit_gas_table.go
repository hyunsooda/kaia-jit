package vm

import "github.com/kaiachain/kaia/params"

// JIT 대상 Opcode들의 가스비 (EVM 스펙 기준)
// 실제 Geth에서는 protocol_params.go 등에 정의되어 있으나, JIT용으로 최적화된 테이블 정의
var jitGasTable = [256]uint64{
	// Arithmetic (Low: 3, Mid: 5, High: 10...)
	0x01: 3, // ADD
	0x02: 5, // MUL
	0x03: 3, // SUB
	0x04: 5, // DIV
	0x05: 5, // SDIV
	0x06: 5, // MOD
	0x07: 5, // SMOD
	0x08: 8, // ADDMOD (Mid)
	0x09: 8, // MULMOD (Mid)
	0x0b: 5, // SIGNEXTEND

	// Comparison (Very Low: 3)
	0x10: 3, 0x11: 3, 0x12: 3, 0x13: 3, 0x14: 3, 0x15: 3, // LT, GT... ISZERO
	0x16: 3, 0x17: 3, 0x18: 3, 0x19: 3, // AND, OR, XOR, NOT...

	// SHA3
	0x20: params.Sha3Gas,

	0x1a: 3, // BYTE (추가)
	0x1b: 3, // SHL  (추가)
	0x1c: 3, // SHR  (추가)
	0x1d: 3, // SAR  (추가)

	// CALLDATALOAD
	0x35: 3,
	// CALLDATASIZE
	0x36: 2,

	// Stack (Very Low: 2 or 3)
	0x50: 2, // POP
	0x5f: 2, // PUSH0 (Shanghai)
	// PUSH1~32 (3)
	// DUP1~16 (3)
	// SWAP1~16 (3)

	0x51: 3,
	0x52: 3,
	0x53: 3,

	// Flow
	0x56: 8,  // JUMP (Mid)
	0x57: 10, // JUMPI (High)
	0x58: 2,  // PC
	0x5b: 1,  // JUMPDEST
}

// init 함수에서 나머지 반복되는 가스비 채움
func init() {
	for i := 0x60; i <= 0x7f; i++ {
		jitGasTable[i] = 3
	} // PUSH
	for i := 0x80; i <= 0x8f; i++ {
		jitGasTable[i] = 3
	} // DUP
	for i := 0x90; i <= 0x9f; i++ {
		jitGasTable[i] = 3
	} // SWAP
}
