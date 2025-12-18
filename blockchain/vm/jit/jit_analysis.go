package jit

import (
	"encoding/binary"
	"fmt"
	"slices"
)

// --- [1] 데이터 구조 정의 ---

type CodeSegment struct {
	Offset uint64
	Length uint64
}

type JitTraceResult struct {
	CanJit         bool
	Segments       []CodeSegment
	NetStackDelta  int // 최종 스택 변화량
	MinStack       int
	MaxStackGrowth int    // 최대 스택 증가량 (Cap 확보용)
	TotalGas       uint64 // [추가] 이 Trace를 실행하는 데 필요한 총 가스비
	NextPC         uint64
}

// Opcode 정보를 담는 메타 데이터
type opInfo struct {
	pops, pushes int
	isSupported  bool
	isStaticPush bool // PUSH0 ~ PUSH32 여부
}

// --- [2] 분석기 상태 관리 (State Machine) ---

type traceAnalyzer struct {
	code     []byte
	pc       uint64
	startPC  uint64
	segments []CodeSegment

	// [추가] 분석 결과 저장소 (여기에 모아서 반환함)
	results BatchAnalysisResult

	visited map[uint64]bool

	// [추가] 흐름이 끊겨서 다음 JUMPDEST를 찾고 있는 중인가?
	scanningForJumpdest bool

	// 스택 상태 추적
	netDelta        int
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
		code: code,
		// pc:        pc,
		// startPC:   pc,
		// segOffset: pc,
		visited:  make(map[uint64]bool),
		results:  make(BatchAnalysisResult),
		worklist: []uint64{pc},
	}
}

// --- [3] 메인 분석 함수 ---

func AnalyzeTrace(code []byte, pc uint64) BatchAnalysisResult {
	analyzer := newTraceAnalyzer(code, pc)
	return analyzer.run()
}

// // tryStaticJump: PUSH + JUMP 패턴을 감지하고 Trace를 잇습니다.
// func (az *traceAnalyzer) tryStaticJump() bool {
// 	// 조건 1: 직전이 PUSH 계열인가?
// 	if !az.isLastOpPush() {
// 		return false
// 	}

// 	// 1. [Check 1] 값이 코드 길이보다 큰가? (거대수 포함)
// 	dest, ok := az.getDestIfValid(az.lastPushData)
// 	if !ok {
// 		return false // 범위 초과
// 	}

// 	// 2. [Check 2] JUMPDEST 확인
// 	// (위에서 범위 체크 끝났으니 인덱싱 안전함)
// 	if az.code[dest] != 0x5b {
// 		return false
// 	}

// 	// 3-1. 이미 분석 완료된 블록인가? -> 연결 불가
// 	if _, exists := az.results[dest]; exists {
// 		return false // Fusion 실패 -> Main loop에서 discard 됨
// 	}

// 	// 3-2. 루프 감지 (현재 경로상에 있는가?) -> 연결 불가
// 	if az.visited[dest] {
// 		return false // Fusion 실패 -> Main loop에서 discard 됨
// 	}

// 	// =========================================================
// 	// [Missing Check] 가스비 누락 방지!
// 	// =========================================================
// 	// JUMP 명령어를 JIT 목록에서 삭제(Fusion)하더라도,
// 	// EVM 스펙상 JUMP 가스비(8)는 소모되어야 합니다.
// 	// 이 함수가 true를 리턴하면 메인 루프의 가스 계산을 건너뛰므로, 여기서 더해줘야 합니다.

// 	az.totalGas += jitGasTable[0x56] // JUMP Gas (8)

// 	// Subtract one becuaase we removed `push` instruction
// 	az.currentRelDepth -= 1

// 	// >>> Optimization: Instruction Fusion <<<
// 	// 직전 PUSH와 현재 JUMP 명령어를 JIT 실행 목록에서 제거함.
// 	// 현재 세그먼트 길이에서 직전 명령어(PUSH) 길이만큼 뺌.
// 	az.segLen -= az.lastInstSize

// 	// 현재까지의 세그먼트 저장 (길이가 0보다 클 때만)
// 	// fmt.Println("S-3")
// 	az.finishSegment()
// 	// fmt.Println("S-4")

// 	// State Update: PUSH(+1) -> JUMP(-1) 이므로 스택 델타는 변화 없음 (그대로 유지)

// 	// PC 점프
// 	az.pc = dest

// 	// 새 세그먼트 시작 준비
// 	az.segOffset = dest
// 	az.segLen = 0
// 	az.resetLastOp() // 점프 직후엔 직전 명령어 정보 초기화

// 	return true
// }

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
	az.netDelta += (info.pushes - info.pops)

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

	// 실제 값 비교: 값이 코드 길이보다 크거나 같으면 -> 탈락!
	if val >= uint64(len(az.code)) {
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

// BatchAnalysisResult: 분석된 JIT 블록들의 집합
// Key: 블록의 시작 PC (uint64)
// Value: JIT 실행에 필요한 메타데이터 (가스, 스택 정보 등)
type BatchAnalysisResult map[uint64]JitTraceResult

// JIT 실행을 위한 최소 바이트코드 길이 임계값
// 너무 짧은 코드(예: PUSH 1개)는 JIT 오버헤드가 더 크므로 무시
// const MinJitBlockSize = 10

// const MinJitBlockSize = 8
const MinJitBlockSize = 5

func (az *traceAnalyzer) prerun() map[uint64]any {
	var (
		pc           = uint64(0)
		allDest      = make(map[uint64]any)
		destRemovals = make(map[uint64]any)
		jumpdests    = make(map[uint64]any)
		pushes       = make(map[uint64]any)
	)
	for pc < uint64(len(az.code)) {
		op := az.code[pc]
		if op >= 0x60 && op <= 0x7f { // PUSH1..32
			dataLen := uint64(op - 0x60 + 1)
			nextPc := pc + dataLen + 1
			if nextPc > uint64(len(az.code)) {
				break
			}
			pushVal := az.code[pc+1 : pc+dataLen+1]
			nextPcOp := az.code[nextPc]
			if nextPcOp == 0x56 || nextPcOp == 0x57 {
				buf := make([]byte, 8)
				copy(buf, pushVal)
				dest := binary.LittleEndian.Uint64(buf)
				destRemovals[dest] = struct{}{}
			} else {
				if dest, ok := az.getDestIfValid(pushVal); ok {
					if az.code[dest] == 0x5b {
						allDest[dest] = struct{}{}
					}
				}
			}

			buf := make([]byte, 8)
			copy(buf, pushVal)
			pushV := binary.LittleEndian.Uint64(buf)
			pushes[pushV] = struct{}{}
		}
		if op == 0x5b {
			jumpdests[pc] = struct{}{}
		}
		pc++
	}
	for dest := range destRemovals {
		delete(allDest, dest)
	}
	// for dest := range pushes {
	// 	if _, exist := jumpdests[dest]; exist {
	// 		fmt.Println("WHAT", dest)
	// 		delete(allDest, dest)
	// 	}
	// }
	return allDest
}

func (az *traceAnalyzer) run() BatchAnalysisResult {
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

	// for visit := range az.visited {
	// 	fmt.Println("VISIT", visit)
	// }

	// for dest := range az.prerun() {
	// 	if dest == 0x4c {
	// 		dest -= 1
	// 	}
	// 	az.worklist = append(az.worklist, dest+1)
	// 	fmt.Printf("DEST: %x\n", dest+1)
	// }
	// for len(az.worklist) > 0 {
	// 	// 1. Pop
	// 	pc := az.worklist[0]
	// 	az.worklist = az.worklist[1:]

	// 	// 2. 유효성 및 중복 방문 체크
	// 	if pc >= uint64(len(az.code)) {
	// 		continue
	// 	}
	// 	if az.visited[pc] {
	// 		continue
	// 	}

	// 	// 3. 새로운 Trace 분석 시작
	// 	az.analyzeSuperBlock(pc)
	// }

	return az.results
}

// analyzeSuperBlock: JUMPDEST를 무시하고 JUMPI/JUMP까지 길게 분석 (Superblock Strategy)
func (az *traceAnalyzer) analyzeSuperBlock(startPC uint64) {
	// 상태 초기화
	az.resetInternalState(startPC)
	az.visited[startPC] = true

	localPath := make(map[uint64]bool)
	localPath[startPC] = true

	enable := false
	if startPC == 0x4d {
		// az.startPC = 0x4c
		fmt.Println("WWW", az.pc, az.segOffset, az.segLen)
		// enable = true
	}

	// 루프: Control Flow가 바뀔 때까지 무한 직진
	for az.pc < uint64(len(az.code)) {
		if enable {
			fmt.Printf("@@@: %x\n", az.pc)
		}
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
					// 루프 발견! 여기서 끊고 Dispatcher에게 넘김
					az.saveSegment(dest)
					// (dest는 이미 path에 있으니 startPC로 등록되어 있거나 worklist에 있을 것임)
					return
				}

				// 가스비 처리 & 명령어 제거
				az.totalGas += jitGasTable[0x56]
				az.currentRelDepth -= 1
				az.segLen -= az.lastInstSize
				az.finishSegment()

				// 방문 여부에 따라 Inlining 결정
				if az.visited[dest] {
					// 이미 방문함 (Loop Back-edge 등) -> 끊고 Link
					az.saveSegment(dest)
				} else {
					if az.pc+1 < uint64(len(az.code)) && az.code[az.pc+1] == 0x5b {
						// if az.pc == 0x4b {
						// 	az.addToWorklist(az.pc + 1)
						// 	fmt.Printf("@@@: %x\n", az.pc+1)
						// } else {
						// 	az.addToWorklist(az.pc + 2)
						// 	fmt.Printf("@@@: %x\n", az.pc+2)
						// }
						az.addToWorklist(az.pc + 2)
						fmt.Printf("@@@: %x\n", az.pc+2)
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
			// az.addToWorklist(az.pc + 1)

			if az.pc+1 < uint64(len(az.code)) && az.code[az.pc+1] == 0x5b {
				az.addToWorklist(az.pc + 2)
				// fmt.Printf("F: %x\n", az.pc+2)
			} else {
				az.addToWorklist(az.pc + 1)
				// fmt.Printf("F: %x\n", az.pc+1)
			}

			// 경로 2: Target (조건 참) - Static일 경우만 추가 가능
			if dest, isStatic := az.checkStaticJump(); isStatic {
				// az.addToWorklist(dest)
				az.addToWorklist(dest + 1)
				// fmt.Printf("T: %x\n", dest+1)
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

// // TODO: valid jump dest가 발견안될경우 핸들링이 현재없는상황 (추후에 넣어야함)
// func (az *traceAnalyzer) run() BatchAnalysisResult {
// 	az.addToWorklist(0)

// 	for az.pc < uint64(len(az.code)) {

// 		op := az.code[az.pc]

// 		// ---------------------------------------------------------
// 		// [Mode 1] JUMPDEST 탐색 모드 (안전지대 찾기)
// 		// ---------------------------------------------------------
// 		// JUMP나 JUMPI 이후에는 다음 코드가 데이터인지 코드인지 모르므로
// 		// JUMPDEST가 나올 때까지 분석을 중단하고 넘어감.
// 		if az.scanningForJumpdest {
// 			if op == 0x5b { // JUMPDEST 발견!
// 				az.scanningForJumpdest = false

// 				// 여기서부터 새로운 블록 분석 시작
// 				// (JUMPDEST는 제거)
// 				// az.prepareNextBlock(az.pc)
// 				az.prepareNextBlock(az.pc + 1)
// 				az.pc++
// 				continue
// 			} else {
// 				// JUMPDEST가 아니면 그냥 건너뜀
// 				az.pc++
// 				continue
// 			}
// 		}

// 		// ---------------------------------------------------------
// 		// [Mode 2] 일반 분석 모드
// 		// ---------------------------------------------------------

// 		// [중복 방지] 이미 분석된 블록의 시작점이면 패스
// 		if _, exists := az.results[az.pc]; exists {
// 			az.discardSegment()
// 			az.scanningForJumpdest = true // 이 블록 끝날 때까지 스킵 유도 (단순화)
// 			az.pc++
// 			continue
// 		}

// 		az.visited[az.pc] = true
// 		info := getOpInfo(op)

// 		// [Case 1] 미지원 Opcode (Unsupported) -> "여기는 끊고, 바로 다음부터 시작"
// 		// (미지원이어도 실행 흐름은 이어지므로 JUMPDEST를 찾을 필요는 없음)
// 		if !info.isSupported {
// 			az.discardSegment() // 오염됐으니 버림
// 			az.pc++
// 			az.prepareNextBlock(az.pc) // 바로 재시작
// 			continue
// 		}

// 		// [Case 2] JUMP (Unconditional)
// 		if op == 0x56 {
// 			// Static Jump (Fusion) -> 연결됨 (계속 분석)
// 			if az.tryStaticJump() {
// 				continue
// 			}

// 			az.saveSegment(az.pc)
// 			// [변경] 바로 다음이 아니라, JUMPDEST 찾으러 떠남
// 			az.pc++
// 			az.scanningForJumpdest = true
// 			continue
// 		}

// 		// [Case 3] JUMPI (Conditional) -> "저장하고, JUMPDEST 찾으러 떠남"
// 		if op == 0x57 {
// 			// NOTE: Do not include `JUMPI` instruction into code segments
// 			az.saveSegment(az.pc) // Fall-through를 NextPC로 저장 (런타임엔 갈 수 있으니까)

// 			// [핵심 변경] JUMPI 뒤는 Dead Code일 수 있음.
// 			// 따라서 무작정 분석하지 말고 다음 JUMPDEST까지 건너뜀.
// 			az.pc++
// 			az.scanningForJumpdest = true
// 			continue
// 		}

// 		// optimization: remove JUMPDEST instruction in compiled JIT version
// 		if op == 0x5b {
// 			// 이전에 모으던 게 있으면 저장
// 			az.finishSegment()

// 			// JUMPDEST 명령어(1바이트) 건너뛰기
// 			az.pc++

// 			// 새 세그먼트 시작 준비
// 			az.segOffset = az.pc
// 			az.segLen = 0

// 			az.lastOp = 0x5b
// 			az.lastPushData = nil
// 			az.lastInstSize = 1

// 			// 방문 체크 (루프 감지용)
// 			az.visited[az.pc-1] = true
// 			continue
// 		}

// 		// [Case 4] 일반 명령어
// 		if !az.finalizeStep(op, info) {
// 			az.discardSegment()
// 			az.pc++
// 			az.prepareNextBlock(az.pc) // 에러는 그냥 리셋 후 재시작
// 			continue
// 		}
// 	}

// 	// 코드 끝
// 	az.saveSegment(az.pc)
// 	return az.results
// }

func (az *traceAnalyzer) checkStaticJump() (uint64, bool) {
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
	az.netDelta = 0
	az.totalGas = 0

	az.resetLastOp()

	// 현재 세그먼트 길이 초기화
	az.segLen = 0
}
