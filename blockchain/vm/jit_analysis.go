package vm

import (
	"encoding/binary"
	"fmt"
	"math/big"
	"slices"
	"strings"
)

// --- [1] 데이터 구조 정의 ---

type CodeSegment struct {
	Offset uint64
	Length uint64
}

type JitTraceResult struct {
	CanJit            bool
	Segments          []CodeSegment
	NetStackDelta     int // 최종 스택 변화량
	NetStackDeltaList []int
	MinStack          int
	MaxStackGrowth    int // 최대 스택 증가량 (Cap 확보용)
	MaxMemoryOff      uint64
	TotalGas          uint64 // [추가] 이 Trace를 실행하는 데 필요한 총 가스비
	NextPC            uint64
}

// Opcode 정보를 담는 메타 데이터
type opInfo struct {
	pops, pushes int
	isSupported  bool
	isStaticPush bool // PUSH0 ~ PUSH32 여부
}

type analysisSlot struct {
	isStatic bool     // PUSH로 만든 상수인가?
	val      *big.Int // 상수값
}

type MemAnalysisSlot struct {
	isStatic bool
	off      uint64
	val      *big.Int
}

// --- [2] 분석기 상태 관리 (State Machine) ---

type traceAnalyzer struct {
	code     []byte
	pc       uint64
	startPC  uint64
	segments []CodeSegment

	// [추가] 분석 결과 저장소 (여기에 모아서 반환함)
	results BatchAnalysisResult

	jumpTargets map[uint64]uint64
	blockStacks map[string][]analysisSlot

	memSlot   map[uint64]MemAnalysisSlot
	blockMems map[string]map[uint64]MemAnalysisSlot

	maxMemoryOff uint64

	visited map[uint64]bool

	jumpDests BitVec

	// 스택 상태 추적
	currentRelDepth int
	minStackDepth   int
	maxGrowth       int
	totalGas        uint64

	// 현재 세그먼트 추적
	segOffset uint64
	segLen    uint64

	// Static Jump 분석용 (직전 명령어 기억)
	lastOp       byte
	lastPushData []byte // PUSH된 데이터 (JUMP 목적지 후보)
	lastInstSize uint64 // 직전 명령어 길이 (Fusion시 삭제용)

	worklist []uint64
}

func newTraceAnalyzer(code []byte, pc uint64) *traceAnalyzer {
	return &traceAnalyzer{
		code:         code,
		jumpDests:    codeBitmap(code),
		jumpTargets:  make(map[uint64]uint64),
		blockStacks:  make(map[string][]analysisSlot),
		memSlot:      make(map[uint64]MemAnalysisSlot),
		blockMems:    make(map[string]map[uint64]MemAnalysisSlot),
		visited:      make(map[uint64]bool),
		results:      make(BatchAnalysisResult),
		maxMemoryOff: 0,
		worklist:     []uint64{pc},
	}
}

// --- [3] 메인 분석 함수 ---

func AnalyzeTrace(code []byte, pc uint64) (BatchAnalysisResult, error) {
	// TODO: if hardfork is scheduled, jump table might need to be re-calcaulated
	analyzer := newTraceAnalyzer(code, pc)
	return analyzer.run()
}

func (az *traceAnalyzer) isValidJumpDest(dest uint64) bool {
	if dest >= uint64(len(az.code)) {
		return false
	}
	if az.code[dest] != 0x5b {
		return false
	}
	return az.jumpDests.codeSegment(dest)
}

// finalizeStep: 스택 계산, PC 이동, 히스토리 기록
func (az *traceAnalyzer) finalizeStep(op byte, info opInfo) bool {
	// 1. 가스비 계산 및 누적
	gas := jitGasTable[op]
	// TODO: 0x5f condition is for hardfork-awareness.
	if gas == 0 && op != 0x00 && op != 0x5f {
		// 테이블에 0으로 되어있는데 STOP(0x00)이 아니면,
		// 우리가 가스비를 정의 안 한 미지원 Opcode일 수 있음 -> 안전하게 JIT 포기
		// (PUSH0 같은 0 cost opcode 제외)
		// 하지만 여기선 PUSH0도 2gas이므로 gas==0이면 미지원으로 간주 가능
		return false
	}
	az.totalGas += gas

	// 2. 스택 시뮬레이션 (메모리 안전용)
	az.currentRelDepth -= info.pops
	// [Check] 지금 바닥을 뚫었나? (최저점 갱신)
	if az.currentRelDepth < az.minStackDepth {
		az.minStackDepth = az.currentRelDepth
	}

	az.currentRelDepth += info.pushes
	// [Check] 지금 천장을 뚫었나? (최고점 갱신)
	if az.currentRelDepth > az.maxGrowth {
		az.maxGrowth = az.currentRelDepth
	}

	// 3. PC 이동 계산
	instSize := uint64(1)
	var pushData []byte

	if info.isStaticPush && op != 0x5f { // PUSH1..32
		dataLen := uint64(op - 0x60 + 1)
		instSize += dataLen

		// 데이터 읽기 (범위 체크)
		if az.pc+instSize > uint64(len(az.code)) {
			return false
		}
		pushData = az.code[az.pc+1 : az.pc+instSize]
	}

	// 3. 상태 업데이트
	az.lastOp = op
	az.lastInstSize = instSize
	az.lastPushData = pushData

	az.segLen += instSize
	az.pc += instSize

	return true
}

func (az *traceAnalyzer) finishSegment() {
	if az.segLen > 0 {
		az.segments = append(az.segments, CodeSegment{
			Offset: az.segOffset,
			Length: az.segLen,
		})
	}
}

// --- [5] 헬퍼 메서드 ---

func (az *traceAnalyzer) isLastOpPush() bool {
	// PUSH0(0x5f) 또는 PUSH1~32(0x60~0x7f)
	return az.lastOp == 0x5f || (az.lastOp >= 0x60 && az.lastOp <= 0x7f)
}

func (az *traceAnalyzer) resetLastOp() {
	az.lastOp = 0
	az.lastPushData = nil
	az.lastInstSize = 0
}

func (az *traceAnalyzer) getDestIfValid(data []byte) (uint64, bool) {
	// 1. 빈 데이터 (PUSH0)
	if len(data) == 0 {
		return 0, true
	}

	// 2. 앞쪽 0 제거 (Trim Leading Zeros)
	// ex: [00, 00, 05] -> [05]
	// ex: [FF, ... ] -> [FF, ... ]
	start := 0
	for start < len(data) && data[start] == 0 {
		start++
	}
	trimmed := data[start:]

	// 3. [Check 1] 값이 코드 길이보다 큰가?

	// 3-A: 유효 숫자 길이가 8바이트보다 길면?
	// -> uint64 최대값보다 큰 수라는 뜻.
	// -> 당연히 코드 길이(최대 24KB)보다 훨씬 큼. -> 탈락!
	if len(trimmed) > 8 {
		return 0, false
	}

	// 3-B: 8바이트 이하면 uint64로 변환해서 직접 비교
	var buf [8]byte
	copy(buf[8-len(trimmed):], trimmed)
	val := binary.BigEndian.Uint64(buf[:])

	if !az.isValidJumpDest(val) {
		return 0, false
	}

	return val, true
}

// getOpInfo: Opcode별 스펙 정의 (Single Source of Truth)
func getOpInfo(op byte) opInfo {
	// jitGasTable은 패키지 레벨 변수로 정의되어 있다고 가정
	if jitGasTable[op] == 0 {
		return opInfo{isSupported: false}
	}

	switch {
	// --- PUSH Operations (isStaticPush = true) ---

	// PUSH0 (Shanghai)
	case op == 0x5f:
		return opInfo{0, 1, true, true}

	// PUSH1 ~ PUSH32
	case op >= 0x60 && op <= 0x7f:
		return opInfo{0, 1, true, true}

	// --- Stack Operations ---

	// DUP1 ~ DUP16
	case op >= 0x80 && op <= 0x8f:
		return opInfo{0, 1, true, false}

	// SWAP1 ~ SWAP16
	case op >= 0x90 && op <= 0x9f:
		return opInfo{0, 0, true, false}

	// POP
	case op == 0x50:
		return opInfo{1, 0, true, false}

	// --- Arithmetic / Logic / Comparison ---
	// 0x01~0x0b (Arith), 0x10~0x1d (Cmp/Bitwise)
	case (op >= 0x01 && op <= 0x0b) || (op >= 0x10 && op <= 0x1d):
		// EXP(0x0a)는 제외 (Dynamic Gas)
		if op == 0x0a {
			return opInfo{isSupported: false}
		}

		// 인자가 1개인 연산 (Pop 1, Push 1)
		// ISZERO(0x15), NOT(0x19)
		if op == 0x15 || op == 0x19 {
			return opInfo{1, 1, true, false}
		}

		// 인자가 3개인 연산 (Pop 3, Push 1)
		// ADDMOD(0x08), MULMOD(0x09)
		if op == 0x08 || op == 0x09 {
			return opInfo{3, 1, true, false}
		}

		// 나머지는 전부 이항 연산 (Pop 2, Push 1)
		// ADD, MUL, SUB, DIV, SDIV, MOD, SMOD, SIGNEXTEND
		// LT, GT, SLT, SGT, EQ, AND, OR, XOR, BYTE, SHL, SHR, SAR
		return opInfo{2, 1, true, false}

	// ---  Environmental Information (CallData) ---
	// CALLDATALOAD (0x35)
	// Stack: Pop 1 (offset), Push 1 (data) -> Net 0
	// Gas: 3 (VeryLow)
	case op == 0x35:
		return opInfo{1, 1, true, false}

	// CALLDATASIZE (0x36)
	// Stack: Pop 0, Push 1 (size) -> Net +1
	// Gas: 2 (Base)
	case op == 0x36:
		return opInfo{0, 1, true, false}

		// MEMORY
	case op == 0x51: // MLOAD
		return opInfo{1, 1, true, false}
	case op == 0x52 || op == 0x53: // MSTORE, MSTORE8
		return opInfo{2, 0, true, false}

	// --- Flow Control ---

	// PC
	case op == 0x58:
		return opInfo{0, 1, true, false}

	// JUMPDEST
	case op == 0x5b:
		return opInfo{0, 0, true, false}

	// JUMP (Unconditional)
	case op == 0x56:
		return opInfo{1, 0, true, false}

	// JUMPI (Conditional)
	case op == 0x57:
		return opInfo{2, 0, true, false}

	default:
		return opInfo{isSupported: false}
	}
}

// Phase 1 전용 Opcode 정보 반환 (모든 명령어 지원)
func getPhase1OpInfo(op byte) opInfo {
	switch {
	// --- 0x00: Stop & Arithmetic ---
	case op == 0x00: // STOP
		return opInfo{0, 0, true, false}
	case op >= 0x01 && op <= 0x0b: // ADD, MUL, SUB ...
		if op == 0x08 || op == 0x09 { // ADDMOD, MULMOD
			return opInfo{3, 1, true, false}
		}
		return opInfo{2, 1, true, false} // ADD~SMOD, EXP, SIGNEXTEND

	// --- 0x10: Comparison & Bitwise ---
	case op >= 0x10 && op <= 0x1d:
		if op == 0x15 || op == 0x19 { // ISZERO, NOT
			return opInfo{1, 1, true, false}
		}
		return opInfo{2, 1, true, false} // LT~SAR

	// --- 0x20: SHA3 ---
	case op == 0x20:
		return opInfo{2, 1, true, false}

	// --- 0x30: Environmental Info ---
	case op >= 0x30 && op <= 0x3f:
		if op == 0x31 || op == 0x3b || op == 0x3f { // BALANCE, EXTCODESIZE, EXTCODEHASH
			return opInfo{1, 1, true, false}
		}
		if op == 0x37 || op == 0x39 || op == 0x3e { // CALLDATACOPY, CODECOPY, RETURNDATACOPY
			return opInfo{3, 0, true, false}
		}
		if op == 0x3c { // EXTCODECOPY
			return opInfo{4, 0, true, false}
		}
		if op == 0x35 { // CALLDATALOAD
			return opInfo{1, 1, true, false}
		}
		// ADDRESS, ORIGIN, CALLER, CALLVALUE, CALLDATASIZE, CODESIZE, GASPRICE, RETURNDATASIZE
		return opInfo{0, 1, true, false}

	// --- 0x40: Block Info ---
	case op >= 0x40 && op <= 0x4a:
		if op == 0x40 || op == 0x49 { // BLOCKHASH, BLOBHASH
			return opInfo{1, 1, true, false}
		}
		// COINBASE, TIMESTAMP, NUMBER, PREVRANDAO, GASLIMIT, CHAINID, SELFBALANCE, BASEFEE, BLOBBASEFEE
		return opInfo{0, 1, true, false}

	// --- 0x50: Stack & Memory & Flow ---
	case op == 0x50: // POP
		return opInfo{1, 0, true, false}
	case op == 0x51: // MLOAD
		return opInfo{1, 1, true, false}
	case op == 0x52 || op == 0x53: // MSTORE, MSTORE8
		return opInfo{2, 0, true, false}
	case op == 0x54: // SLOAD
		return opInfo{1, 1, true, false}
	case op == 0x55: // SSTORE
		return opInfo{2, 0, true, false}
	case op == 0x56: // JUMP
		return opInfo{1, 0, true, false}
	case op == 0x57: // JUMPI
		return opInfo{2, 0, true, false}
	case op == 0x58: // PC
		return opInfo{0, 1, true, false}
	case op == 0x59: // MSIZE
		return opInfo{0, 1, true, false}
	case op == 0x5a: // GAS
		return opInfo{0, 1, true, false}
	case op == 0x5b: // JUMPDEST
		return opInfo{0, 0, true, false}
	case op == 0x5c: // TLOAD (Cancun)
		return opInfo{1, 1, true, false}
	case op == 0x5d: // TSTORE (Cancun)
		return opInfo{2, 0, true, false}
	case op == 0x5e: // MCOPY (Cancun)
		return opInfo{3, 0, true, false}
	case op == 0x5f: // PUSH0 (Shanghai)
		return opInfo{0, 1, true, true} // Static Push

	// --- 0x60: PUSH ---
	case op >= 0x60 && op <= 0x7f: // PUSH1 ~ PUSH32
		return opInfo{0, 1, true, true} // Static Push

	// --- 0x80: DUP ---
	case op >= 0x80 && op <= 0x8f: // DUP1 ~ DUP16
		// DUP은 Pop하지 않고(0), 하나 더 얹음(1).
		// *주의*: Underflow 체크(깊이 n개 확인)는 Loop 안에서 별도로 수행해야 함.
		return opInfo{0, 1, true, false}

	// --- 0x90: SWAP ---
	case op >= 0x90 && op <= 0x9f: // SWAP1 ~ SWAP16
		// SWAP은 교체만 하므로 스택 높이 변화 없음.
		// *주의*: Underflow 체크(깊이 n+1개 확인)는 Loop 안에서 별도로 수행해야 함.
		return opInfo{0, 0, true, false}

	// --- 0xA0: LOG ---
	case op >= 0xa0 && op <= 0xa4: // LOG0 ~ LOG4
		// LOGn pops 2 + n items
		return opInfo{2 + int(op-0xa0), 0, true, false}

	// --- 0xF0: System ---
	case op == 0xf0: // CREATE
		return opInfo{3, 1, true, false}
	case op == 0xf1 || op == 0xf2: // CALL, CALLCODE
		return opInfo{7, 1, true, false}
	case op == 0xf3: // RETURN
		return opInfo{2, 0, true, false}
	case op == 0xf4 || op == 0xfa: // DELEGATECALL, STATICCALL
		return opInfo{6, 1, true, false}
	case op == 0xf5: // CREATE2
		return opInfo{4, 1, true, false}
	case op == 0xfd: // REVERT
		return opInfo{2, 0, true, false}
	case op == 0xfe: // INVALID
		return opInfo{0, 0, false, false}
	case op == 0xff: // SELFDESTRUCT
		return opInfo{1, 0, true, false}

	default:
		// 정의되지 않은 Opcode
		return opInfo{0, 0, false, false}
	}
}

// BatchAnalysisResult: 분석된 JIT 블록들의 집합
// Key: 블록의 시작 PC (uint64)
// Value: JIT 실행에 필요한 메타데이터 (가스, 스택 정보 등)
type BatchAnalysisResult map[uint64]JitTraceResult

// JIT 실행을 위한 최소 바이트코드 길이 임계값
// 너무 짧은 코드(예: PUSH 1개)는 JIT 오버헤드가 더 크므로 무시
// const MinJitBlockSize = 10

// const MinJitBlockSize = 8
const MinJitBlockSize = 5

func (az *traceAnalyzer) run() (BatchAnalysisResult, error) {
	if err := az.runPhase1(az.worklist[0]); err != nil {
		return nil, err
	}

	// Worklist가 빌 때까지 반복 (BFS/DFS)
	for len(az.worklist) > 0 {
		// 1. Pop
		pc := az.worklist[0]
		az.worklist = az.worklist[1:]

		// 2. 유효성 및 중복 방문 체크
		if pc >= uint64(len(az.code)) {
			continue
		}
		if az.visited[pc] {
			continue
		}

		// 3. 새로운 Trace 분석 시작
		az.analyzeSuperBlock(pc)
	}

	return az.results, nil
}

// analyzeSuperBlock: JUMPDEST를 무시하고 JUMPI/JUMP까지 길게 분석 (Superblock Strategy)
func (az *traceAnalyzer) analyzeSuperBlock(startPC uint64) {
	// 상태 초기화
	az.resetInternalState(startPC)
	az.visited[startPC] = true

	localPath := make(map[uint64]bool)
	localPath[startPC] = true

	// 루프: Control Flow가 바뀔 때까지 무한 직진
	for az.pc < uint64(len(az.code)) {
		op := az.code[az.pc]

		// ---------------------------------------------------------
		// [1] JUMPDEST (0x5b) 처리
		// ---------------------------------------------------------
		// Superblock 전략: JUMPDEST에서 블록을 끊지 않고 계속 잇습니다.
		// 외부에서 들어오는 분기점일 수 있지만, 현재 Trace 관점에서는 단순 통과점입니다.
		if op == 0x5b {
			az.finishSegment() // Offset 조정을 위해 끊어줌
			az.pc++            // JUMPDEST(1byte) 건너뛰기
			az.segOffset = az.pc
			az.segLen = 0
			az.resetLastOp()
			continue
		}

		info := getOpInfo(op)

		if op == 0x51 || op == 0x52 || op == 0x53 {
			memSlot := az.memSlot[az.pc]
			if memSlot.isStatic && memSlot.off > az.maxMemoryOff {
				az.maxMemoryOff = memSlot.off
			} else {
				info.isSupported = false
			}
		}

		// ---------------------------------------------------------
		// [2] 미지원 Opcode (Unsupported)
		// ---------------------------------------------------------
		// JIT는 여기서 멈추지만, 인터프리터 실행 후 다음 명령어부터
		// 다시 JIT가 가능할 수 있으므로 Next PC를 Worklist에 추가합니다.
		if !info.isSupported {
			az.saveSegment(az.pc) // NextPC = 현재 위치 (Interpreter 진입점)
			// az.addToWorklist(az.pc + 1)

			if az.pc+1 < uint64(len(az.code)) && az.code[az.pc+1] == 0x5b {
				az.addToWorklist(az.pc + 2)
			} else {
				az.addToWorklist(az.pc + 1)
			}
			return
		}

		// ---------------------------------------------------------
		// [3] JUMP (Unconditional)
		// ---------------------------------------------------------
		if op == 0x56 {
			// Static Jump (Fusion) 확인
			dest, isStatic := az.checkStaticJump()

			if isStatic {
				// [Static Jump] -> Inlining 시도

				// 1. 내 꼬리를 물었나? (Infinite Loop 방지)
				if localPath[dest] {
					panic("TODO: Remove me")
					// 루프 발견! 여기서 끊고 Dispatcher에게 넘김
					az.saveSegment(dest)
					// (dest는 이미 path에 있으니 startPC로 등록되어 있거나 worklist에 있을 것임)
					return
				}

				// 가스비 처리 & 명령어 제거
				az.totalGas += jitGasTable[0x56]
				az.currentRelDepth -= 1

				// az.segLen -= az.lastInstSize
				if az.lastOp >= 0x60 && az.lastOp <= 0x7f {
					az.segLen -= az.lastInstSize
				} else {
					az.segLen += 1
				}
				az.finishSegment()

				// 방문 여부에 따라 Inlining 결정
				if az.visited[dest] {
					// 이미 방문함 (Loop Back-edge 등) -> 끊고 Link
					az.saveSegment(dest)
				} else {
					if az.pc+1 < uint64(len(az.code)) && az.code[az.pc+1] == 0x5b {
						az.addToWorklist(az.pc + 2)
					} else {
						az.addToWorklist(az.pc + 1)
					}
					// 처음 방문함 -> Inlining (이어 붙이기)
					az.pc = dest
					az.segOffset = dest
					az.segLen = 0
					az.resetLastOp()
					localPath[dest] = true

					continue // Worklist 추가 없이 직접 이동
				}
			} else {
				// [Dynamic Jump] -> 분석 불가, 여기서 종료
				az.saveSegment(az.pc)

				// [수정] Dynamic Jump라도 혹시 모를 Fall-through나
				// 다른 경로에서의 진입을 위해 다음 PC를 Worklist에 추가
				if az.pc+1 < uint64(len(az.code)) && az.code[az.pc+1] == 0x5b {
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
		if op == 0x57 {
			// JUMPI는 실행 흐름 분기점이므로 Trace 종료
			az.saveSegment(az.pc)

			// [수정] Dynamic Target일 수도 있으므로, Fall-through는 무조건 추가

			// 경로 1: Fall-through (조건 거짓)
			if az.pc+1 < uint64(len(az.code)) && az.code[az.pc+1] == 0x5b {
				az.addToWorklist(az.pc + 2)
			} else {
				az.addToWorklist(az.pc + 1)
			}

			// 경로 2: Target (조건 참) - Static일 경우만 추가 가능
			if dest, isStatic := az.checkStaticJump(); isStatic && az.isValidJumpDest(dest) {
				az.addToWorklist(dest + 1)
			}

			return
		}

		// [5] 일반 명령어 (Normal Execution)
		if !az.finalizeStep(op, info) {
			az.discardSegment()
			return
		}
	}

	// 코드 끝(EOF)에 도달
	az.saveSegment(az.pc)
}

type Worklist struct {
	startPC uint64
	srcs    string
}

func copyMemSlot(memSlotFrom, memSlotTo map[uint64]MemAnalysisSlot) {
	for off, slot := range memSlotFrom {
		newSlot := MemAnalysisSlot{isStatic: slot.isStatic, off: slot.off}
		if slot.val != nil {
			newSlot.val = new(big.Int).Set(slot.val)
		}
		memSlotTo[off] = newSlot
	}
}

func (az *traceAnalyzer) runPhase1(startPC uint64) error {
	// 초기 상태: 시작점 스택은 비어있음
	az.blockStacks[fmt.Sprintf("%x", startPC)] = []analysisSlot{}
	az.blockMems[fmt.Sprintf("%x", startPC)] = make(map[uint64]MemAnalysisSlot)
	var (
		worklist = []Worklist{{startPC: startPC, srcs: fmt.Sprintf("%x", startPC)}}
		jumpSrcs = make(map[uint64]map[uint64]bool)
		// Helper: 스택 전파 (Deep Copy)
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

		propagateMem = func(srcPC, targetPC uint64, originSrcs string, memSlot map[uint64]MemAnalysisSlot) {
			newMemSlot := make(map[uint64]MemAnalysisSlot)
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
		memSlot := make(map[uint64]MemAnalysisSlot)
		copyMemSlot(az.blockMems[srcs], memSlot)

		// 블록 순회
		for pc < uint64(len(az.code)) {
			op := az.code[pc]
			info := getPhase1OpInfo(op)

			// 미지원 Opcode -> 여기서 끊김
			if !info.isSupported {
				panic("TODO: Remove me")
				break
			}
			// 명령어 길이
			instSize := uint64(1)
			if op >= 0x60 && op <= 0x7f {
				instSize += uint64(op - 0x60 + 1)
			}

			// Stack Underflow Check
			if len(stack) < info.pops {
				panic("TODO: Remove me 1")
				break
			}

			arithFunc := func(fn func(*big.Int, *big.Int) (*big.Int, bool)) {
				s1, s2 := stack[len(stack)-1], stack[len(stack)-2] // Top, Second
				stack = stack[:len(stack)-2]
				if s1.isStatic && s2.isStatic {
					if res, ok := fn(s1.val, s2.val); ok {
						stack = append(stack, analysisSlot{true, res})
						return
					}
				}
				stack = append(stack, analysisSlot{isStatic: false})
			}
			switch {
			case op == 0x01:
				arithFunc(safeAdd)
			case op == 0x02:
				arithFunc(safeMul)
			case op == 0x03:
				arithFunc(safeSub)
			case op == 0x16:
				arithFunc(safeAnd)
			case op == 0x1b:
				arithFunc(safeShl)

			// MLOAD
			case op == 0x51:
				off := stack[len(stack)-1]
				stack[len(stack)-1] = analysisSlot{isStatic: false} // pre-assigment. will be updated if possible
				if off.isStatic {
					if _, ok := safeAdd(off.val, big.NewInt(32)); ok {
						// TODO: add memory size overflow check
						var val *big.Int
						isStatic := false
						if memSlot[off.val.Uint64()].isStatic {
							val = memSlot[off.val.Uint64()].val
							isStatic = true
						}
						memSlot[off.val.Uint64()] = MemAnalysisSlot{isStatic: isStatic, off: off.val.Uint64(), val: val}
						az.memSlot[pc] = memSlot[off.val.Uint64()]
						stack[len(stack)-1] = analysisSlot{isStatic: isStatic, val: val}
					}
				}

			// MSTORE, MSTORE8
			case op == 0x52 || op == 0x53:
				off := stack[len(stack)-1]
				val := stack[len(stack)-2]
				stack = stack[:len(stack)-2]
				if off.isStatic && val.isStatic {
					valStore := val.val
					if op == 0x53 { // MSTORE8
						valStore = new(big.Int).And(valStore, big.NewInt(0xFF))
					}
					memSlot[off.val.Uint64()] = MemAnalysisSlot{isStatic: true, off: off.val.Uint64(), val: valStore}
					az.memSlot[pc] = memSlot[off.val.Uint64()]
				}

			case op == 0x56: // JUMP
				target := stack[len(stack)-1]     // Top 확인
				nextStack := stack[:len(stack)-1] // Pop Address
				if _, exist := jumpSrcs[pc]; !exist {
					jumpSrcs[pc] = make(map[uint64]bool)
				}
				jumpSrcs[pc][target.val.Uint64()] = true

				if target.isStatic {
					dest := target.val.Uint64()
					// 유효성 체크
					if az.isValidJumpDest(dest) {
						if !isLastOpPush {
							az.jumpTargets[pc] = dest // [기록] Phase 2에서 사용
						}
						propagateStack(pc, dest, srcs, nextStack)
						propagateMem(pc, dest, srcs, memSlot)
					}
				}
				// Do not add a `pc+1` to worklist because the stack integrity is not gauratneed for next instruction when `jump` is not effective
				// propagateStack(pc, pc+1, srcs, nextStack)
				// propagateMem(pc, pc+1, srcs, memSlot)
				isLastOpPush = false
				goto StopBlock // 블록 종료

			case op == 0x57: // JUMPI
				target := stack[len(stack)-1]     // 2nd Item
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

			case op >= 0x80 && op <= 0x8f: // DUP
				n := int(op - 0x80 + 1)
				if len(stack) < n {
					panic("TODO: Remove me 2")
					goto StopBlock
				}
				stack = append(stack, stack[len(stack)-n])
				isLastOpPush = false

			case op >= 0x90 && op <= 0x9f: // SWAP
				n := int(op - 0x90 + 1)
				if len(stack) < n+1 {
					panic("TODO: Remove me 3")
					goto StopBlock
				}
				topIdx := len(stack) - 1
				swapIdx := len(stack) - 1 - n
				stack[topIdx], stack[swapIdx] = stack[swapIdx], stack[topIdx]
				isLastOpPush = false

			case op >= 0x60 && op <= 0x7f: // PUSH
				data := az.code[pc+1 : pc+instSize]
				// slot := analysisSlot{isStatic: false}

				// TODO: Remove me
				// // 8바이트 이하 & 코드 범위 내 -> Static
				// if len(data) <= 8 {
				// 	var buf [8]byte
				// 	copy(buf[8-len(data):], data)
				// 	val := binary.BigEndian.Uint64(buf[:])

				// 	if val < uint64(len(az.code)) {
				// 		slot = analysisSlot{isStatic: true, val: val}
				// 	} else {
				// 		fmt.Printf("KKKK: %x\n", pc)
				// 	}
				// }

				slot := analysisSlot{isStatic: true, val: new(big.Int).SetBytes(data)}
				stack = append(stack, slot)
				isLastOpPush = true

			case op == 0x5f: // PUSH0
				isLastOpPush = false
				stack = append(stack, analysisSlot{isStatic: true, val: nil})

			case op == 0x00 || op == 0xf3 || op == 0xfd: // STOP, RETURN, REVERT
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

		// PUSH1(0x60) ~ PUSH32(0x7f) 인지 확인
		if op >= 0x60 && op <= 0x7f {
			dataLen := uint64(op - 0x60 + 1)
			// 다음 명령어 위치 = 현재위치 + 1(Opcode) + 데이터길이
			nextPC := pc + 1 + dataLen

			// 만약 건너뛴 위치가 코드 끝을 넘어가면 중단
			if nextPC >= uint64(len(az.code)) {
				pc = nextPC
				break
			}
			pc = nextPC
		} else {
			// PUSH가 아니면 루프 종료 (스킵 완료)
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

	// 목적지가 유효하고 JUMPDEST인지 확인
	if dest >= uint64(len(az.code)) || az.code[dest] != 0x5b {
		return 0, false
	}

	return dest, true
}

// saveSegment: 현재까지 모은 세그먼트들이 유효하고 길다면 결과 맵에 저장
func (az *traceAnalyzer) saveSegment(nextPC uint64) {
	// [핵심 추가] 1. 현재 모으고 있던 조각(Pending)이 있다면 슬라이스에 추가 (Flush)
	// fmt.Println("S-1")
	az.finishSegment()
	// fmt.Println("S-2")

	// 1. 세그먼트가 없으면 저장할 필요 없음
	if len(az.segments) == 0 {
		return
	}

	// 2. [임계값 체크] JIT 할 가치가 있는 길이인지 확인
	totalLen := uint64(0)
	for _, s := range az.segments {
		totalLen += s.Length
	}

	if totalLen < MinJitBlockSize {
		// if totalLen != 10 {
		// if totalLen != MinJitBlockSize {
		// 너무 짧으면 그냥 버림 (인터프리터가 실행하는 게 나음)
		az.discardSegment()
		return
	}

	// 3. 결과 구조체 생성
	res := JitTraceResult{
		CanJit: true,
		// 슬라이스 복사 (Deep Copy) - az.segments는 재사용되므로 복사 필수
		Segments: slices.Clone(az.segments),

		TotalGas:      az.totalGas,
		NetStackDelta: az.currentRelDepth, // 최종 상대 높이가 곧 순수 변화량

		// [핵심] 최저점이 -5였다면, 실행 전 최소 5개가 필요함
		// minStackDepth는 항상 0 이하의 음수이므로 부호를 반대로 뒤집음
		MinStack: -az.minStackDepth,

		MaxStackGrowth: az.maxGrowth,
		MaxMemoryOff:   az.maxMemoryOff + 32,
		NextPC:         nextPC,
	}

	// 4. 결과 맵에 등록 (Key: 이 블록의 최초 시작점)
	az.results[az.startPC] = res

	// 5. 저장했으므로 내부 버퍼 비우기 (다음 블록 준비)
	az.segments = az.segments[:0]
}

// discardSegment: 현재 모으던 블록을 폐기 (오염되었거나, 스티칭 실패 시)
func (az *traceAnalyzer) discardSegment() {
	az.segments = az.segments[:0]
}

// // prepareNextBlock: 새로운 JIT 블록 탐색을 위한 시작점 설정
// // (미지원 명령어 등을 건너뛴 직후 호출됨)
// func (az *traceAnalyzer) prepareNextBlock(newStartPC uint64) {
// 	// 내부 상태(스택 시뮬레이션 등) 리셋
// 	az.resetInternalState()

// 	// if new start PC is JUMPDEST, then skip it
// 	if az.code[newStartPC] == 0x5b {
// 		newStartPC++
// 	}

// 	// 시작점 정보 갱신
// 	az.startPC = newStartPC
// 	az.segOffset = newStartPC
// }

// resetInternalState: 누적된 시뮬레이션 상태 변수 초기화
func (az *traceAnalyzer) resetInternalState(startPC uint64) {
	// 세그먼트 리스트 비움
	az.segments = az.segments[:0] // GC 효율을 위해 capacity는 유지하고 len만 0으로 (az.segments[:0]) 해도 됨

	az.pc = startPC
	az.startPC = startPC
	az.segOffset = startPC

	// 스택 시뮬레이터 초기화 (새 블록은 0부터 시작)
	az.currentRelDepth = 0
	az.minStackDepth = 0
	az.maxGrowth = 0
	az.totalGas = 0
	az.maxMemoryOff = 0

	az.resetLastOp()

	// 현재 세그먼트 길이 초기화
	az.segLen = 0
}

// TODO: Add overflow check
func safeAdd(top, sec *big.Int) (*big.Int, bool) {
	return new(big.Int).Add(top, sec), true
}

// TODO: Add overflow check
func safeMul(top, sec *big.Int) (*big.Int, bool) {
	return new(big.Int).Mul(top, sec), true
}

// TODO: Add overflow check
func safeSub(top, sec *big.Int) (*big.Int, bool) {
	return new(big.Int).Sub(top, sec), true
}

// TODO: Add overflow check
func safeAnd(top, sec *big.Int) (*big.Int, bool) {
	return new(big.Int).And(top, sec), true
}

// TODO: Add overflow check
func safeShl(top, sec *big.Int) (*big.Int, bool) {
	return new(big.Int).Lsh(sec, uint(top.Uint64())), true
}
