package vm

import (
	"encoding/binary"
	"fmt"
	"slices"
	"strings"

	"github.com/holiman/uint256"
	"github.com/kaiachain/kaia/common/math"
	"github.com/kaiachain/kaia/params"
)

// --- [1] 데이터 구조 정의 ---

type codeSegment struct {
	Offset uint64
	Length uint64
}

type JitTraceResult struct {
	Segments          []codeSegment
	NetStackDelta     int //  Final stack delta
	NetStackDeltaList []int
	MinStack          int // Minimum stack growth
	MaxStackGrowth    int // Maximum stack growth (for `cap()`)
	MaxMemoryOff      uint64
	TotalGas          uint64
	NextPC            uint64
}

type opInfo struct {
	pops, pushes int
	isSupported  bool
	isStaticPush bool // PUSH1 ~ PUSH32
}

type analysisSlot struct {
	isStatic bool
	val      *uint256.Int
}

type memAnalysisSlot struct {
	isStatic bool
	off      uint64
	length   uint64
	val      *uint256.Int
}

type traceAnalyzer struct {
	code     []byte
	pc       uint64
	startPC  uint64
	segments []codeSegment

	results BatchAnalysisResult

	jumpTargets map[uint64]uint64
	blockStacks map[string][]analysisSlot

	memSlot   map[uint64]memAnalysisSlot
	blockMems map[string]map[uint64]memAnalysisSlot

	maxMemoryOff uint64

	visited map[uint64]bool

	jumpDests BitVec

	// Track stack diff
	currentRelDepth int
	minStackDepth   int
	maxGrowth       int
	totalGas        uint64

	// Track current segment
	segOffset uint64
	segLen    uint64

	// Metadata for static jump analysis (PUSH + JUMP)
	lastOp       byte
	lastPushData []byte
	lastInstSize uint64

	worklist []uint64
}

func newTraceAnalyzer(code []byte, pc uint64) *traceAnalyzer {
	return &traceAnalyzer{
		code:         code,
		jumpDests:    codeBitmap(code),
		jumpTargets:  make(map[uint64]uint64),
		blockStacks:  make(map[string][]analysisSlot),
		memSlot:      make(map[uint64]memAnalysisSlot),
		blockMems:    make(map[string]map[uint64]memAnalysisSlot),
		visited:      make(map[uint64]bool),
		results:      make(BatchAnalysisResult),
		maxMemoryOff: 0,
		worklist:     []uint64{pc},
	}
}

func AnalyzeTrace(code []byte, pc uint64) (BatchAnalysisResult, error) {
	// TODO: if hardfork is scheduled, jump table might need to be re-calcaulated
	analyzer := newTraceAnalyzer(code, pc)
	return analyzer.run()
}

func (az *traceAnalyzer) isValidJumpDest(dest uint64) bool {
	if dest >= uint64(len(az.code)) {
		return false
	}
	if az.code[dest] != byte(JUMPDEST) {
		return false
	}
	return az.jumpDests.codeSegment(dest)
}

// finalizeStep: 스택 계산, PC 이동, 히스토리 기록
func (az *traceAnalyzer) finalizeStep(op byte, info opInfo) bool {
	// 1. 가스비 계산 및 누적
	gas := jitGasTable[op]
	// TODO: 0x5f condition is for hardfork-awareness.
	if gas == 0 && op != byte(STOP) && op != byte(PUSH0) {
		// If the gas cost is 0 in the table but the opcode is not STOP (0x00),
		// it may be an unsupported opcode with an undefined gas cost.
		// To be safe, we abort JIT compilation in this case.
		// (Note: Since PUSH0 is assigned a 2-gas cost here, any gas == 0 can be treated as unsupported.)
		return false
	}

	// SHA3 (0x20) 추가 가스비 (Word Cost)
	if op == byte(SHA3) {
		if slot, exists := az.memSlot[az.pc]; exists && slot.isStatic {
			// toWordSize: (len + 31) / 32
			wordCount := toWordSize(slot.length)
			// 6 * wordCount
			extraGas, overflow := math.SafeMul(wordCount, params.Sha3WordGas)
			if overflow {
				return false
			}
			// 기본 가스에 추가
			gas, overflow = math.SafeAdd(gas, extraGas)
			if overflow {
				return false
			}
		} else {
			// SHA3인데 길이가 동적이면 JIT 불가
			return false
		}
	}

	az.totalGas += gas

	// 2. Track stack delta
	az.currentRelDepth -= info.pops
	// [Check] Hit a new low? (New floor)
	if az.currentRelDepth < az.minStackDepth {
		az.minStackDepth = az.currentRelDepth
	}

	az.currentRelDepth += info.pushes
	// [Check] Hit a new high? (New ceiling)
	if az.currentRelDepth > az.maxGrowth {
		az.maxGrowth = az.currentRelDepth
	}

	// 3. Calculate next PC (i.e., consider the size of the `PUSH` data)
	instSize := uint64(1)
	var pushData []byte

	if info.isStaticPush && op != byte(PUSH0) { // PUSH1..32
		dataLen := uint64(op - byte(PUSH1) + 1)
		instSize += dataLen

		// 데이터 읽기 (범위 체크)
		if az.pc+instSize > uint64(len(az.code)) {
			return false
		}
		pushData = az.code[az.pc+1 : az.pc+instSize]
	}

	// 3. Update the stack tracking metadata
	az.lastOp = op
	az.lastInstSize = instSize
	az.lastPushData = pushData

	az.segLen += instSize
	az.pc += instSize

	return true
}

func (az *traceAnalyzer) finishSegment() {
	if az.segLen > 0 {
		az.segments = append(az.segments, codeSegment{
			Offset: az.segOffset,
			Length: az.segLen,
		})
	}
}

func (az *traceAnalyzer) isLastOpPush() bool {
	// PUSH0 or PUSH1-32
	return az.lastOp == byte(PUSH0) || (az.lastOp >= byte(PUSH1) && az.lastOp <= byte(PUSH32))
}

func (az *traceAnalyzer) resetLastOp() {
	az.lastOp = 0
	az.lastPushData = nil
	az.lastInstSize = 0
}

func (az *traceAnalyzer) getDestIfValid(data []byte) (uint64, bool) {
	if len(data) == 0 {
		return 0, true
	}

	// Trim Leading Zeros
	// ex: [00, 00, 05] -> [05]
	// ex: [FF, ... ] -> [FF, ... ]
	start := 0
	for start < len(data) && data[start] == 0 {
		start++
	}
	trimmed := data[start:]

	// valid jumpdest never be exceed of the valid value within 8 byte
	if len(trimmed) > 8 {
		return 0, false
	}

	var buf [8]byte
	copy(buf[8-len(trimmed):], trimmed)
	val := binary.BigEndian.Uint64(buf[:])

	if !az.isValidJumpDest(val) {
		return 0, false
	}

	return val, true
}

func getOpInfo(op byte) opInfo {
	// if no gas is specified, it's unsupported opcode
	if jitGasTable[op] == 0 {
		return opInfo{isSupported: false}
	}

	switch {
	// PUSH0 (Shanghai)
	case op == byte(PUSH0):
		return opInfo{0, 1, true, true}

	// PUSH1 ~ PUSH32
	case op >= byte(PUSH1) && op <= byte(PUSH32):
		return opInfo{0, 1, true, true}

	// --- Stack Operations ---

	// DUP1 ~ DUP16
	case op >= byte(DUP1) && op <= byte(DUP16):
		return opInfo{0, 1, true, false}

	// SWAP1 ~ SWAP16
	case op >= byte(SWAP1) && op <= byte(SWAP16):
		return opInfo{0, 0, true, false}

	// POP
	case op == byte(POP):
		return opInfo{1, 0, true, false}

	// --- Arithmetic / Logic / Comparison ---
	// 0x01~0x0b (Arith), 0x10~0x1d (Cmp/Bitwise)
	case (op >= byte(ADD) && op <= byte(SIGNEXTEND)) || (op >= byte(LT) && op <= byte(SAR)):
		// No suupoort of `Exp` opcode due to the dynamic cost
		if op == byte(EXP) {
			return opInfo{isSupported: false}
		}

		if op == byte(ISZERO) || op == byte(NOT) {
			return opInfo{1, 1, true, false}
		}

		if op == byte(ADDMOD) || op == byte(MULMOD) {
			return opInfo{3, 1, true, false}
		}

		// ADD, MUL, SUB, DIV, SDIV, MOD, SMOD, SIGNEXTEND
		// LT, GT, SLT, SGT, EQ, AND, OR, XOR, BYTE, SHL, SHR, SAR
		return opInfo{2, 1, true, false}

	case op == byte(SHA3):
		return opInfo{2, 1, true, false}

	// ---  Environmental Information (CallData) ---
	// Stack: Pop 1 (offset), Push 1 (data) -> Net 0
	// Gas: 3 (VeryLow)
	case op == byte(CALLDATALOAD):
		return opInfo{1, 1, true, false}

	// Stack: Pop 0, Push 1 (size) -> Net +1
	// Gas: 2 (Base)
	case op == byte(CALLDATASIZE):
		return opInfo{0, 1, true, false}

	case op == byte(MLOAD): // MLOAD
		return opInfo{1, 1, true, false}
	case op == byte(MSTORE) || op == byte(MSTORE8):
		return opInfo{2, 0, true, false}

	// --- Flow Control ---

	case op == byte(PC):
		return opInfo{0, 1, true, false}
	case op == byte(JUMPDEST):
		return opInfo{0, 0, true, false}
	case op == byte(JUMP):
		return opInfo{1, 0, true, false}
	case op == byte(JUMPI):
		return opInfo{2, 0, true, false}

	default:
		return opInfo{isSupported: false}
	}
}

// getPhase1OpInfo supports all opcodes to support static analysis for stack and memory propgatation
func getPhase1OpInfo(op byte) opInfo {
	switch {
	// --- 0x00: Stop & Arithmetic ---
	case op == byte(STOP): // STOP
		return opInfo{0, 0, true, false}
	case op >= byte(ADD) && op <= byte(SIGNEXTEND): // ADD, MUL, SUB ...
		if op == byte(ADDMOD) || op == byte(MULMOD) { // ADDMOD, MULMOD
			return opInfo{3, 1, true, false}
		}
		return opInfo{2, 1, true, false} // ADD~SMOD, EXP, SIGNEXTEND

	// --- 0x10: Comparison & Bitwise ---
	case op >= byte(LT) && op <= byte(SAR):
		if op == byte(ISZERO) || op == byte(NOT) {
			return opInfo{1, 1, true, false}
		}
		return opInfo{2, 1, true, false} // LT~SAR

	case op == byte(SHA3):
		return opInfo{2, 1, true, false}

	// --- 0x30: Environmental Info ---
	case op >= byte(ADDRESS) && op <= byte(EXTCODEHASH):
		if op == byte(BALANCE) || op == byte(EXTCODESIZE) || op == byte(EXTCODEHASH) {
			return opInfo{1, 1, true, false}
		}
		if op == byte(CALLDATACOPY) || op == byte(CODECOPY) || op == byte(RETURNDATACOPY) {
			return opInfo{3, 0, true, false}
		}
		if op == byte(EXTCODECOPY) {
			return opInfo{4, 0, true, false}
		}
		if op == byte(CALLDATALOAD) {
			return opInfo{1, 1, true, false}
		}
		// ADDRESS, ORIGIN, CALLER, CALLVALUE, CALLDATASIZE, CODESIZE, GASPRICE, RETURNDATASIZE
		return opInfo{0, 1, true, false}

	// --- 0x40: Block Info ---
	case op >= byte(BLOCKHASH) && op <= byte(BLOBBASEFEE):
		if op == byte(BLOCKHASH) || op == byte(BLOCKHASH) {
			return opInfo{1, 1, true, false}
		}
		// COINBASE, TIMESTAMP, NUMBER, PREVRANDAO, GASLIMIT, CHAINID, SELFBALANCE, BASEFEE, BLOBBASEFEE
		return opInfo{0, 1, true, false}

	// --- 0x50: Stack & Memory & Flow ---
	case op == byte(POP):
		return opInfo{1, 0, true, false}
	case op == byte(MLOAD):
		return opInfo{1, 1, true, false}
	case op == byte(MSTORE) || op == byte(MSTORE8):
		return opInfo{2, 0, true, false}
	case op == byte(SLOAD):
		return opInfo{1, 1, true, false}
	case op == byte(SSTORE):
		return opInfo{2, 0, true, false}
	case op == byte(JUMP):
		return opInfo{1, 0, true, false}
	case op == byte(JUMPI):
		return opInfo{2, 0, true, false}
	case op == byte(PC):
		return opInfo{0, 1, true, false}
	case op == byte(MSIZE):
		return opInfo{0, 1, true, false}
	case op == byte(GAS):
		return opInfo{0, 1, true, false}
	case op == byte(JUMPDEST):
		return opInfo{0, 0, true, false}
	case op == byte(TLOAD):
		return opInfo{1, 1, true, false}
	case op == byte(TSTORE):
		return opInfo{2, 0, true, false}
	case op == byte(MCOPY):
		return opInfo{3, 0, true, false}
	case op == byte(PUSH0):
		return opInfo{0, 1, true, true} // Static Push

	// --- 0x60: PUSH ---
	case op >= byte(PUSH1) && op <= byte(PUSH32): // PUSH1 ~ PUSH32
		return opInfo{0, 1, true, true} // Static Push

	// --- 0x80: DUP ---
	case op >= byte(DUP1) && op <= byte(DUP16): // DUP1 ~ DUP16
		return opInfo{0, 1, true, false}

	// --- 0x90: SWAP ---
	case op >= byte(SWAP1) && op <= byte(SWAP16): // SWAP1 ~ SWAP16
		return opInfo{0, 0, true, false}

	// --- 0xA0: LOG ---
	case op >= byte(LOG0) && op <= byte(LOG4): // LOG0 ~ LOG4
		// LOGn pops 2 + n items
		return opInfo{2 + int(op-byte(LOG0)), 0, true, false}

	// --- 0xF0: System ---
	case op == byte(CREATE):
		return opInfo{3, 1, true, false}
	case op == byte(CALL) || op == byte(CALLCODE):
		return opInfo{7, 1, true, false}
	case op == byte(RETURN):
		return opInfo{2, 0, true, false}
	case op == byte(DELEGATECALL) || op == byte(STATICCALL):
		return opInfo{6, 1, true, false}
	case op == byte(CREATE2):
		return opInfo{4, 1, true, false}
	case op == byte(REVERT):
		return opInfo{2, 0, true, false}
	case op == 0xfe: // INVALID
		return opInfo{0, 0, false, false}
	case op == byte(SELFDESTRUCT):
		return opInfo{1, 0, true, false}

	default:
		// 정의되지 않은 Opcode
		return opInfo{0, 0, false, false}
	}
}

// BatchAnalysisResult: set of compilable code
// key: start PC
// value: metadata of compilable code
type BatchAnalysisResult map[uint64]JitTraceResult

// Minimum bytecode length threshold for JIT execution
// const MinJitBlockSize = 20
// const MinJitBlockSize = 10
// // const MinJitBlockSize = 8
const MinJitBlockSize = 5

func (az *traceAnalyzer) run() (BatchAnalysisResult, error) {
	if err := az.runPhase1(az.worklist[0]); err != nil {
		return nil, err
	}

	cnt := 0
	for _, s := range az.memSlot {
		if s.isStatic {
			cnt++
		}
	}
	fmt.Println("M", cnt)

	// Worklist: BFS
	for len(az.worklist) > 0 {
		// 1. Pop
		pc := az.worklist[0]
		az.worklist = az.worklist[1:]

		// TODO: Remove me
		// // 2. 유효성 및 중복 방문 체크
		// if pc >= uint64(len(az.code)) {
		// 	fmt.Println("??")
		// 	continue
		// }
		if az.visited[pc] {
			continue
		}

		az.analyzeSuperBlock(pc)
	}

	return az.results, nil
}

// analyzeSuperBlock collects compilable code
func (az *traceAnalyzer) analyzeSuperBlock(startPC uint64) {
	// 상태 초기화
	az.resetInternalState(startPC)
	az.visited[startPC] = true

	localPath := make(map[uint64]bool)
	localPath[startPC] = true

	for az.pc < uint64(len(az.code)) {
		op := az.code[az.pc]

		// ---------------------------------------------------------
		// [1] JUMPDEST
		// ---------------------------------------------------------
		// Superblock strategy: Continue tracing without breaking the block at JUMPDEST.
		if op == byte(JUMPDEST) {
			az.finishSegment() // Offset 조정을 위해 끊어줌
			az.pc++            // JUMPDEST(1byte) 건너뛰기
			az.segOffset = az.pc
			az.segLen = 0
			az.resetLastOp()
			continue
		}

		info := getOpInfo(op)

		if op == byte(MLOAD) || op == byte(MSTORE) || op == byte(MSTORE8) || op == byte(SHA3) {
			memSlot := az.memSlot[az.pc]

			if memSlot.isStatic {
				var dataLen uint64
				switch op {
				case byte(MLOAD), byte(MSTORE):
					dataLen = 32
				case byte(MSTORE8):
					dataLen = 1
				}

				reqEnd, overflow := math.SafeAdd(memSlot.off, dataLen)
				if overflow {
					info.isSupported = false
				} else {
					wordCount := toWordSize(reqEnd)
					memorySize, overflow := math.SafeMul(wordCount, 32)
					if overflow {
						info.isSupported = false
					} else {
						if memorySize > az.maxMemoryOff {
							az.maxMemoryOff = memorySize
						}
					}
				}
			} else {
				// if the memory offset is not able to calculate statically, we don't jit
				info.isSupported = false
			}
		}
		// 여기로 자연스럽게 내려와서 이후의 segment 저장 로직이 실행됨

		// ---------------------------------------------------------
		// [2] Unsupported Opcode
		// ---------------------------------------------------------
		// JIT stops here, but execution may resume from the next instruction via the interpreter.
		// Add the Next PC to the worklist to allow for future JIT compilation.
		if !info.isSupported {
			az.saveSegment(az.pc)
			if az.pc+1 < uint64(len(az.code)) && az.code[az.pc+1] == byte(JUMPDEST) {
				az.addToWorklist(az.pc + 2)
			} else {
				az.addToWorklist(az.pc + 1)
			}
			return
		}

		// ---------------------------------------------------------
		// [3] JUMP (Unconditional)
		// ---------------------------------------------------------
		if op == byte(JUMP) {
			// Check static Jump (Opcode fusion)
			dest, isStatic := az.checkStaticJump()

			if isStatic {
				// Is it for loop?: then we exit the current analysis loop to avoid infinite analysis
				if localPath[dest] {
					panic("TODO: Remove me")
					az.saveSegment(dest)
					return
				}

				// Gas consumption and remove opcode (Opcode Fusion)
				az.totalGas += jitGasTable[byte(JUMP)]
				az.currentRelDepth -= 1

				if az.lastOp >= byte(PUSH1) && az.lastOp <= byte(PUSH32) {
					az.segLen -= az.lastInstSize
				} else {
					az.segLen += 1
				}
				az.finishSegment()

				if az.visited[dest] {
					az.saveSegment(dest)
				} else {
					if az.pc+1 < uint64(len(az.code)) && az.code[az.pc+1] == byte(JUMPDEST) {
						az.addToWorklist(az.pc + 2)
					} else {
						az.addToWorklist(az.pc + 1)
					}
					// Inlining (code segement stitching)
					az.pc = dest
					az.segOffset = dest
					az.segLen = 0
					az.resetLastOp()
					localPath[dest] = true

					continue // No add worklist; continue on the current segement analysis
				}
			} else {
				// Dynamic jump: Not able to stich. Exit the current analysis here
				az.saveSegment(az.pc)

				// Fall-through: add the next PC to the worklist to allow entry from other paths.
				if az.pc+1 < uint64(len(az.code)) && az.code[az.pc+1] == byte(JUMPDEST) {
					az.addToWorklist(az.pc + 2)
				} else {
					az.addToWorklist(az.pc + 1)
				}
			}
			return
		}

		// ---------------------------------------------------------
		// [4] JUMPI (Conditional)
		// ---------------------------------------------------------
		if op == byte(JUMPI) {
			// JUMPI is a control flow branch; terminating the trace.
			az.saveSegment(az.pc)

			// Fall-through: false path
			if az.pc+1 < uint64(len(az.code)) && az.code[az.pc+1] == byte(JUMPDEST) {
				az.addToWorklist(az.pc + 2)
			} else {
				az.addToWorklist(az.pc + 1)
			}

			// Fall-through: true path (if and only if statically calculated)
			if dest, isStatic := az.checkStaticJump(); isStatic && az.isValidJumpDest(dest) {
				az.addToWorklist(dest + 1)
			}

			return
		}

		// [5] Normal opcode (supported opcode)
		if !az.finalizeStep(op, info) {
			// TODO: if `false` is returned, no compile artifact should be created for the current superblock
			az.discardSegment()
			return
		}
	}

	// EOF
	az.saveSegment(az.pc)
}

type Worklist struct {
	startPC uint64
	srcs    string
}

func copyMemSlot(memSlotFrom, memSlotTo map[uint64]memAnalysisSlot) {
	for off, slot := range memSlotFrom {
		newSlot := memAnalysisSlot{isStatic: slot.isStatic, off: slot.off}
		if slot.val != nil {
			newSlot.val = slot.val.Clone()
		}
		memSlotTo[off] = newSlot
	}
}

func (az *traceAnalyzer) runPhase1(startPC uint64) error {
	// Initial state: empty stack and empty memory
	az.blockStacks[fmt.Sprintf("%x", startPC)] = []analysisSlot{}
	az.blockMems[fmt.Sprintf("%x", startPC)] = make(map[uint64]memAnalysisSlot)
	var (
		worklist = []Worklist{{startPC: startPC, srcs: fmt.Sprintf("%x", startPC)}}
		jumpSrcs = make(map[uint64]map[uint64]bool)
		// Helper: Stack propagation (Deep Copy)
		propagateStack = func(srcPC, targetPC uint64, originSrcs string, stack []analysisSlot) {
			newStack := make([]analysisSlot, len(stack))
			if strings.Contains(originSrcs, fmt.Sprintf("%x", targetPC)) {
				return
			}
			copy(newStack, stack)
			srcs := fmt.Sprintf("%s-%x-%x", originSrcs, srcPC, targetPC)
			az.blockStacks[srcs] = newStack
			worklist = append(worklist, Worklist{startPC: targetPC, srcs: srcs})
		}

		// Helper: Memory propagation (Deep Copy)
		propagateMem = func(srcPC, targetPC uint64, originSrcs string, memSlot map[uint64]memAnalysisSlot) {
			newMemSlot := make(map[uint64]memAnalysisSlot)
			if strings.Contains(originSrcs, fmt.Sprintf("%x", targetPC)) {
				return
			}
			copyMemSlot(memSlot, newMemSlot)
			srcs := fmt.Sprintf("%s-%x-%x", originSrcs, srcPC, targetPC)
			az.blockMems[srcs] = newMemSlot
		}
	)

	isLastOpPush := false
	for len(worklist) > 0 {
		var (
			pc   = worklist[0].startPC
			srcs = worklist[0].srcs
		)
		worklist = worklist[1:]
		// stack copy
		stack := make([]analysisSlot, len(az.blockStacks[srcs]))
		copy(stack, az.blockStacks[srcs])

		// memory copy
		memSlot := make(map[uint64]memAnalysisSlot)
		copyMemSlot(az.blockMems[srcs], memSlot)

		for pc < uint64(len(az.code)) {
			op := az.code[pc]
			info := getPhase1OpInfo(op)

			if !info.isSupported {
				panic("TODO: Remove me")
				break
			}
			instSize := uint64(1)
			if op >= byte(PUSH1) && op <= byte(PUSH32) {
				instSize += uint64(op - byte(PUSH1) + 1)
			}

			// Stack Underflow Check
			if len(stack) < info.pops {
				panic("TODO: Remove me 1")
				break
			}

			unaryFunc := func(fn func(*uint256.Int) *uint256.Int) {
				s1 := stack[len(stack)-1] // Top
				stack = stack[:len(stack)-1]
				if s1.isStatic {
					res := fn(s1.val)
					stack = append(stack, analysisSlot{true, res})
					return
				}
				stack = append(stack, analysisSlot{isStatic: false})
			}

			binaryFunc := func(fn func(*uint256.Int, *uint256.Int) *uint256.Int) {
				s1, s2 := stack[len(stack)-1], stack[len(stack)-2] // Top, Second
				stack = stack[:len(stack)-2]
				if s1.isStatic && s2.isStatic {
					res := fn(s1.val, s2.val)
					stack = append(stack, analysisSlot{true, res})
					return
				}
				stack = append(stack, analysisSlot{isStatic: false})
			}

			ternaryFunc := func(fn func(*uint256.Int, *uint256.Int, *uint256.Int) *uint256.Int) {
				s1 := stack[len(stack)-1]    // Top (N)
				s2 := stack[len(stack)-2]    // b
				s3 := stack[len(stack)-3]    // a
				stack = stack[:len(stack)-3] // Pop 3

				if s1.isStatic && s2.isStatic && s3.isStatic {
					res := fn(s1.val, s2.val, s3.val) // fn(a, b, N)
					stack = append(stack, analysisSlot{true, res})
					return
				}
				stack = append(stack, analysisSlot{isStatic: false})
			}
			// TODO: Support more opcodes to wide the possible range of statically calculatable memory values
			switch {
			// --- Arithmetic (Binary) ---
			case op == byte(ADD):
				binaryFunc(Add)
			case op == byte(MUL):
				binaryFunc(Mul)
			case op == byte(SUB):
				binaryFunc(Sub)
			case op == byte(DIV):
				binaryFunc(Div)
			case op == byte(SDIV):
				binaryFunc(SDiv)
			case op == byte(MOD):
				binaryFunc(Mod)
			case op == byte(SMOD):
				binaryFunc(SMod)
			case op == byte(ADDMOD):
				ternaryFunc(AddMod)
			case op == byte(MULMOD):
				ternaryFunc(MulMod)
			case op == byte(SIGNEXTEND):
				// SIGNEXTEND(b, x): Stack Top(b), Second(x)
				// binaryFunc는 Top(s1), Second(s2)를 꺼내서 fn(s1, s2)를 호출함.
				// EVM 스펙상 순서는 Stack[0]=b, Stack[1]=x.
				// 아래 구현된 SignExtend 함수 파라미터 순서에 유의.
				binaryFunc(SignExtend)

			// --- Comparison (Binary) ---
			case op == byte(LT):
				binaryFunc(Lt)
			case op == byte(GT):
				binaryFunc(Gt)
			case op == byte(SLT):
				binaryFunc(Slt)
			case op == byte(SGT):
				binaryFunc(Sgt)
			case op == byte(EQ):
				binaryFunc(Eq)
			case op == byte(ISZERO):
				unaryFunc(IsZero)

			// --- Bitwise (Binary/Unary) ---
			case op == byte(AND):
				binaryFunc(And)
			case op == byte(OR):
				binaryFunc(Or)
			case op == byte(XOR):
				binaryFunc(Xor)
			case op == byte(NOT):
				unaryFunc(Not)
			case op == byte(BYTE): // BYTE [Updated: Unary]
				// 님 요청대로 Unary Operation으로 처리
				// Stack: [..., value] -> [..., byte(value)]
				unaryFunc(Byte)
			case op == byte(SHL):
				binaryFunc(Shl)
			case op == byte(SHR):
				binaryFunc(Shr)
			case op == byte(SAR):
				binaryFunc(Sar)

			// MLOAD
			case op == byte(MLOAD):
				off := stack[len(stack)-1]
				stack[len(stack)-1] = analysisSlot{isStatic: false} // pre-assigment. will be updated if possible
				if off.isStatic {
					if _, overflow := math.SafeAdd(off.val.Uint64(), 32); !overflow {
						// TODO: add memory size overflow check
						var val *uint256.Int
						isStatic := false
						if memSlot[off.val.Uint64()].isStatic {
							val = memSlot[off.val.Uint64()].val
							isStatic = true
						}
						memSlot[off.val.Uint64()] = memAnalysisSlot{isStatic: isStatic, off: off.val.Uint64(), val: val}
						az.memSlot[pc] = memSlot[off.val.Uint64()]
						stack[len(stack)-1] = analysisSlot{isStatic: isStatic, val: val}
					}
				}

			// MSTORE, MSTORE8
			case op == byte(MSTORE) || op == byte(MSTORE8):
				off := stack[len(stack)-1]
				val := stack[len(stack)-2]
				stack = stack[:len(stack)-2]
				if off.isStatic && val.isStatic {
					var (
						valStore = val.val.Clone()
						dataLen  uint64
					)
					if op == byte(MSTORE) { // MSTORE
						dataLen = 32
					} else { // MSTORE8
						dataLen = 1
						valStore.And(valStore, uint256.NewInt(0xFF)) // 하위 1바이트만 유지
					}
					memSlot[off.val.Uint64()] = memAnalysisSlot{
						isStatic: true,
						off:      off.val.Uint64(),
						length:   dataLen,
						val:      valStore,
					}
					az.memSlot[pc] = memSlot[off.val.Uint64()]
				}

			// [수정] SHA3 (0x20) - 가스비 계산용 Offset/Length 추적에만 집중
			// 메모리 내용(Value) 복원은 포기하고 결과는 항상 Dynamic으로 처리
			case op == byte(SHA3):
				size := stack[len(stack)-2]
				offset := stack[len(stack)-1]
				stack = stack[:len(stack)-2] // Pop 2

				// 1. 결과값은 항상 동적(Dynamic)으로 처리 (값 계산 포기)
				// 이렇게 하면 뒤따르는 로직들은 SHA3 결과를 모르는 상태로 진행됨
				stack = append(stack, analysisSlot{isStatic: false})

				// 2. 입력값(Offset, Size)이 정적이라면 가스비 계산을 위해 기록
				if offset.isStatic && size.isStatic {
					start := offset.val.Uint64()
					length := size.val.Uint64()

					// 오버플로우 체크 (유효한 메모리 범위인지 확인)
					if _, overflow := math.SafeAdd(start, length); !overflow {
						// 3. 메모리 사용 정보 기록
						// 값(val)이나 구체적인 데이터는 몰라도 됨.
						// 이 정보는 나중에 analyzeSuperBlock에서:
						// 1) Memory Expansion Gas 계산
						// 2) SHA3 Word Gas (6 * words) 계산
						// 에 사용됩니다.
						az.memSlot[pc] = memAnalysisSlot{
							isStatic: true,
							off:      start,
							length:   length,
							val:      nil, // 내용은 추적하지 않음
						}
					}
				}

			// // SHA3
			// case op == 0x20:
			// 	size := stack[len(stack)-2]
			// 	offset := stack[len(stack)-1]
			// 	stack = stack[:len(stack)-2] // Pop 2

			// 	var (
			// 		hashResult *uint256.Int
			// 		isStatic   = false
			// 	)
			// 	if offset.isStatic && size.isStatic {
			// 		start := offset.val.Uint64()
			// 		length := size.val.Uint64()
			// 		if _, overflow := math.SafeAdd(start, length); !overflow {
			// 			var (
			// 				data    = make([]byte, length)
			// 				valid   = true
			// 				current = uint64(0)
			// 			)

			// 			for current < length {
			// 				slot, exists := memSlot[start+current]
			// 				if !exists || !slot.isStatic || slot.val == nil || slot.length == 0 {
			// 					fmt.Println("@@@", start+current, length, exists, slot.isStatic, slot.val, slot.length)
			// 					for p := range memSlot {
			// 						fmt.Println("!!!", p)
			// 					}
			// 					valid = false
			// 					break
			// 				}
			// 				chunkSize := slot.length // 32(MSTORE) or 1(MSTORE8)
			// 				valBytes := slot.val.Bytes32()
			// 				// 데이터 추출 시작 지점 계산
			// 				// MSTORE (len=32): 0번 인덱스부터 사용
			// 				// MSTORE8 (len=1): 31번 인덱스(LSB)만 사용
			// 				srcStart := 32 - chunkSize

			// 				// 복사할 길이 계산 (남은 길이가 청크보다 작을 수 있음)
			// 				bytesToCopy := chunkSize
			// 				if current+bytesToCopy > length {
			// 					bytesToCopy = length - current
			// 				}
			// 				copy(data[current:], valBytes[srcStart:srcStart+bytesToCopy])
			// 				current += bytesToCopy
			// 			}
			// 			if valid {
			// 				hasher := sha3.NewLegacyKeccak256()
			// 				hasher.Write(data)
			// 				hashResult = new(uint256.Int).SetBytes(hasher.Sum(nil))
			// 				isStatic = true
			// 			}
			// 		}
			// 		// 5. 메모리 사용 정보 기록 (Gas 계산용)
			// 		// 해시 계산 성공 여부와 관계없이, 정적인 Offset/Size 접근은 기록해야 함
			// 		az.memSlot[pc] = memAnalysisSlot{
			// 			isStatic: isStatic,
			// 			off:      start,
			// 			length:   length,
			// 			val:      nil,
			// 		}
			// 	}
			// 	// 6. 결과 스택에 Push
			// 	stack = append(stack, analysisSlot{isStatic: isStatic, val: hashResult})

			case op == byte(JUMP):
				target := stack[len(stack)-1]
				nextStack := stack[:len(stack)-1]
				if _, exist := jumpSrcs[pc]; !exist {
					jumpSrcs[pc] = make(map[uint64]bool)
				}
				jumpSrcs[pc][target.val.Uint64()] = true

				if target.isStatic {
					dest := target.val.Uint64()
					if az.isValidJumpDest(dest) {
						if !isLastOpPush {
							az.jumpTargets[pc] = dest // Will be used for Phase 2 (Superblock analysis)
						}
						propagateStack(pc, dest, srcs, nextStack)
						propagateMem(pc, dest, srcs, memSlot)
					}
				}
				// Do not add a `pc+1` to worklist because the stack integrity is not gauratneed for next instruction when `jump` is not effective
				// propagateStack(pc, pc+1, srcs, nextStack)
				// propagateMem(pc, pc+1, srcs, memSlot)
				isLastOpPush = false
				goto StopBlock

			case op == byte(JUMPI):
				target := stack[len(stack)-1]
				nextStack := stack[:len(stack)-2] // Pop 2

				// 1. Jump Path
				if target.isStatic {
					dest := target.val.Uint64()
					if az.isValidJumpDest(dest) {
						propagateStack(pc, dest, srcs, nextStack)
						propagateMem(pc, dest, srcs, memSlot)
					}
				}
				// 2. Fall-through Path
				nextPC := pc + instSize
				if nextPC < uint64(len(az.code)) {
					propagateStack(pc, nextPC, srcs, nextStack)
					propagateMem(pc, nextPC, srcs, memSlot)
				}
				isLastOpPush = false
				goto StopBlock

			case op >= byte(DUP1) && op <= byte(DUP16):
				n := int(op - byte(DUP1) + 1)
				if len(stack) < n {
					panic("TODO: Remove me 2")
					goto StopBlock
				}
				stack = append(stack, stack[len(stack)-n])
				isLastOpPush = false

			case op >= byte(SWAP1) && op <= byte(SWAP16):
				n := int(op - byte(SWAP1) + 1)
				if len(stack) < n+1 {
					panic("TODO: Remove me 3")
					goto StopBlock
				}
				topIdx := len(stack) - 1
				swapIdx := len(stack) - 1 - n
				stack[topIdx], stack[swapIdx] = stack[swapIdx], stack[topIdx]
				isLastOpPush = false

			case op >= byte(PUSH1) && op <= byte(PUSH32): // PUSH
				data := az.code[pc+1 : pc+instSize]
				// slot := analysisSlot{isStatic: true, val: new(big.Int).SetBytes(data)}
				slot := analysisSlot{isStatic: true, val: new(uint256.Int).SetBytes(data)}
				stack = append(stack, slot)
				isLastOpPush = true

			case op == byte(PUSH0): // PUSH0
				isLastOpPush = false
				stack = append(stack, analysisSlot{isStatic: true, val: nil})

			case op == byte(STOP) || op == byte(RETURN) || op == byte(REVERT):
				isLastOpPush = false
				goto StopBlock

			default: // Others (ADD, etc.) -> 값은 Unknown
				stack = stack[:len(stack)-info.pops]
				for i := 0; i < info.pushes; i++ {
					stack = append(stack, analysisSlot{isStatic: false})
				}
				isLastOpPush = false
			}
			pc += instSize
		}
	StopBlock:
	}

	for pc, destMap := range jumpSrcs {
		if len(destMap) > 1 {
			if _, exist := az.jumpTargets[pc]; exist {
				delete(az.jumpTargets, pc)
			}
		}
	}
	return nil
}

func (az *traceAnalyzer) addToWorklist(pc uint64) {
	// 유효 범위 및 방문 여부 체크
	for pc < uint64(len(az.code)) {
		op := az.code[pc]

		if op >= byte(PUSH1) && op <= byte(PUSH32) {
			dataLen := uint64(op - byte(PUSH1) + 1)
			nextPC := pc + 1 + dataLen
			if nextPC >= uint64(len(az.code)) {
				pc = nextPC
				break
			}
			pc = nextPC
		} else {
			break
		}
	}
	if pc < uint64(len(az.code)) && !az.visited[pc] {
		az.worklist = append(az.worklist, pc)
	}
}

func (az *traceAnalyzer) checkStaticJump() (uint64, bool) {
	// case1: dynamic jump
	if dest, exist := az.jumpTargets[az.pc]; exist {
		return dest, true
	}
	// case2: dynamic jump
	if !az.isLastOpPush() {
		return 0, false
	}
	dest, ok := az.getDestIfValid(az.lastPushData)
	if !ok {
		return 0, false
	}
	if dest >= uint64(len(az.code)) || az.code[dest] != byte(JUMPDEST) {
		return 0, false
	}
	return dest, true
}

// saveSegment: 현재까지 모은 세그먼트들이 유효하고 길다면 결과 맵에 저장
func (az *traceAnalyzer) saveSegment(nextPC uint64) {
	// Flush the current segment
	az.finishSegment()

	// 1. if no segement, return early
	if len(az.segments) == 0 {
		return
	}

	// 2. Threshold Check: Ensure the bytecode is long enough to justify JIT.
	totalLen := uint64(0)
	for _, s := range az.segments {
		totalLen += s.Length
	}

	if totalLen < MinJitBlockSize {
		az.discardSegment()
		return
	}

	// 3. Finalize the JIT trace result
	res := JitTraceResult{
		// Deep copy the slice - Mandatory since az.segments is reused.
		Segments: slices.Clone(az.segments),

		TotalGas:      az.totalGas,
		NetStackDelta: az.currentRelDepth,

		// Since minStackDepth is always 0 or negative, negate it
		MinStack: -az.minStackDepth,

		MaxStackGrowth: az.maxGrowth,
		MaxMemoryOff:   az.maxMemoryOff,
		NextPC:         nextPC,
	}

	az.results[az.startPC] = res
	// 4. Clear the segment and prepare for next block analysis
	az.segments = az.segments[:0]
}

func (az *traceAnalyzer) discardSegment() {
	az.segments = az.segments[:0]
}

func (az *traceAnalyzer) resetInternalState(startPC uint64) {
	az.segments = az.segments[:0]

	az.pc = startPC
	az.startPC = startPC
	az.segOffset = startPC

	az.currentRelDepth = 0
	az.minStackDepth = 0
	az.maxGrowth = 0
	az.totalGas = 0
	az.maxMemoryOff = 0

	az.resetLastOp()

	az.segLen = 0
}

// ============================================================================
// Arithmetic Operations (Return *uint256.Int only)
// ============================================================================

func Add(top, sec *uint256.Int) *uint256.Int {
	return new(uint256.Int).Add(top, sec)
}

func Mul(top, sec *uint256.Int) *uint256.Int {
	return new(uint256.Int).Mul(top, sec)
}

func Sub(top, sec *uint256.Int) *uint256.Int {
	// EVM: Stack[0] - Stack[1]
	return new(uint256.Int).Sub(top, sec)
}

func Div(top, sec *uint256.Int) *uint256.Int {
	// EVM: Stack[0] / Stack[1]
	if sec.IsZero() {
		return new(uint256.Int) // 0
	}
	return new(uint256.Int).Div(top, sec)
}

func Mod(top, sec *uint256.Int) *uint256.Int {
	if sec.IsZero() {
		return new(uint256.Int)
	}
	return new(uint256.Int).Mod(top, sec)
}

func SDiv(top, sec *uint256.Int) *uint256.Int {
	// EVM: Stack[0](top) / Stack[1](sec) (Signed)
	if sec.IsZero() {
		return new(uint256.Int) // 0
	}
	return new(uint256.Int).SDiv(top, sec)
}

func SMod(top, sec *uint256.Int) *uint256.Int {
	if sec.IsZero() {
		return new(uint256.Int)
	}
	return new(uint256.Int).SMod(top, sec)
}

func SignExtend(b, x *uint256.Int) *uint256.Int {
	// EVM: SIGNEXTEND(b, x) -> extends length of x to (b+1) bytes
	// binaryFunc에서 호출 시: top=b, sec=x
	return new(uint256.Int).ExtendSign(x, b)
}

func AddMod(a, b, mod *uint256.Int) *uint256.Int {
	if mod.IsZero() {
		return new(uint256.Int)
	}
	return new(uint256.Int).AddMod(a, b, mod)
}

func MulMod(a, b, mod *uint256.Int) *uint256.Int {
	if mod.IsZero() {
		return new(uint256.Int)
	}
	return new(uint256.Int).MulMod(a, b, mod)
}

// ============================================================================
// Bitwise Operations
// ============================================================================

func Byte(val *uint256.Int) *uint256.Int {
	ret := new(uint256.Int).Set(val)
	return ret.And(ret, uint256.NewInt(0xFF))
}

func And(top, sec *uint256.Int) *uint256.Int {
	return new(uint256.Int).And(top, sec)
}

func Or(top, sec *uint256.Int) *uint256.Int {
	return new(uint256.Int).Or(top, sec)
}

func Xor(top, sec *uint256.Int) *uint256.Int {
	return new(uint256.Int).Xor(top, sec)
}

func Not(top *uint256.Int) *uint256.Int {
	return new(uint256.Int).Not(top)
}

// ============================================================================
// Shift Operations (Stack[0]=ShiftAmount, Stack[1]=Value)
// ============================================================================

func Shl(top, sec *uint256.Int) *uint256.Int {
	if top.LtUint64(256) {
		return new(uint256.Int).Lsh(sec, uint(top.Uint64()))
	}
	return new(uint256.Int)
}

func Shr(top, sec *uint256.Int) *uint256.Int {
	if top.LtUint64(256) {
		return new(uint256.Int).Rsh(sec, uint(top.Uint64()))
	}
	return new(uint256.Int)
}

func Sar(top, sec *uint256.Int) *uint256.Int {
	if top.LtUint64(256) {
		return new(uint256.Int).SRsh(sec, uint(top.Uint64()))
	}
	// Shift >= 256: Signed check
	if sec.Sign() < 0 {
		return new(uint256.Int).SetAllOne() // -1
	}
	return new(uint256.Int) // 0
}

// ============================================================================
// Comparison Operations
// ============================================================================

var (
	u256Zero = new(uint256.Int)
	u256One  = uint256.NewInt(1)
)

func Lt(top, sec *uint256.Int) *uint256.Int {
	if top.Lt(sec) {
		return u256One.Clone()
	}
	return u256Zero.Clone()
}

func Gt(top, sec *uint256.Int) *uint256.Int {
	if top.Gt(sec) {
		return u256One.Clone()
	}
	return u256Zero.Clone()
}

func Eq(top, sec *uint256.Int) *uint256.Int {
	if top.Eq(sec) {
		return u256One.Clone()
	}
	return u256Zero.Clone()
}

func Slt(top, sec *uint256.Int) *uint256.Int {
	if top.Slt(sec) {
		return u256One.Clone()
	}
	return u256Zero.Clone()
}

func Sgt(top, sec *uint256.Int) *uint256.Int {
	if top.Sgt(sec) {
		return u256One.Clone()
	}
	return u256Zero.Clone()
}

func IsZero(top *uint256.Int) *uint256.Int {
	if top.IsZero() {
		return u256One.Clone()
	}
	return u256Zero.Clone()
}
