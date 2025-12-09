package vm

import (
	"fmt"
	"sync"
	"unsafe"

	"github.com/kaiachain/kaia/blockchain/vm/jit"
	"github.com/kaiachain/kaia/common"
)

/*
#include <stdint.h>
#include <stdlib.h>

// [Rust JIT Compiler Function]
// Input:
//   - engine: Rust Engine Pointer
//   - code:   Stitched Bytecode Array
//   - pc_map: Original PC Mapping Array (Code index -> Real PC)
//   - len:    Length of code (and pc_map)
// Output:
//   - Pointer to the generated machine code
void* compile_trace(void* engine, const uint8_t* code, const uint64_t* pc_map, size_t len);
*/
import "C"

// 캐시 키: 컨트랙트 코드 해시 + PC 위치
type jitCacheKey struct {
	codeHash common.Hash
	pc       uint64
}

// 캐시 값: 컴파일된 함수 포인터 + 실행 검증용 메타데이터
type jitCacheValue struct {
	fnPtr unsafe.Pointer // Rust JIT 함수 포인터

	// 검증 및 실행용 데이터
	TotalGas       uint64
	MinStack       int
	MaxStackGrowth int
	NetStackDelta  int
	NextPC         uint64
}

const JIT_FLAG = byte(0xEE)

var (
	// 포인터를 저장하도록 변경 (*jitCacheValue)
	jitCache             = make(map[jitCacheKey]*jitCacheValue)
	jitCompiledContracts = make(map[common.Address][]byte)
	analyzedContracts    = make(map[common.Hash]bool)
	cacheLock            sync.RWMutex
)

// [Phase 1] 실행 전 준비 (Run 함수 도입부에서 호출)
// 컨트랙트 코드 전체를 분석하여 JIT 가능한 모든 블록을 미리 컴파일하고 캐싱합니다.
func (in *EVMInterpreter) PrepareJit(contract *Contract) {
	codeHash := contract.CodeHash

	// 1. [Fast Check] 이미 분석된 컨트랙트인지 확인
	cacheLock.RLock()
	analyzed := analyzedContracts[codeHash]
	cacheLock.RUnlock()

	if analyzed {
		return // 이미 분석 끝난 놈이다. (JIT 블록이 있든 없든)
	}

	// 2. 전체 코드 분석 (Batch Analysis)
	// 0번지부터 시작해서 모든 도달 가능한 JIT 블록을 찾아냄
	batchResults := jit.AnalyzeTrace(contract.Code, 0)
	analyzedContracts[codeHash] = true

	if len(batchResults) == 0 {
		return // JIT 가능한 구간 없음
	}

	// 3. 엔진 초기화
	in.initJitEngine()
	if in.jitEngine == nil {
		return
	}

	// 4. 컴파일 및 등록 (Batch Compile)
	cacheLock.Lock()
	defer cacheLock.Unlock()

	for startPC, trace := range batchResults {

		if startPC == 55 {
			continue
		}

		key := jitCacheKey{codeHash, startPC}

		// 이미 있으면 스킵
		if _, ok := jitCache[key]; ok {
			continue
		}

		// --- Stitching ---
		totalLen := 0
		for _, seg := range trace.Segments {
			totalLen += int(seg.Length)
		}

		linearCode := make([]byte, 0, totalLen)
		linearPC := make([]uint64, 0, totalLen)

		for _, seg := range trace.Segments {
			start := seg.Offset
			end := seg.Offset + seg.Length
			linearCode = append(linearCode, contract.Code[start:end]...)
			for i := uint64(0); i < seg.Length; i++ {
				linearPC = append(linearPC, start+i)
			}
		}

		// --- Compile ---
		if len(linearCode) == 0 {
			continue
		}

		rawPtr := C.compile_trace(
			in.jitEngine,
			(*C.uint8_t)(unsafe.Pointer(&linearCode[0])),
			(*C.uint64_t)(unsafe.Pointer(&linearPC[0])),
			C.size_t(len(linearCode)),
		)

		if rawPtr == nil {
			continue
		}

		// --- Save ---
		jitCache[key] = &jitCacheValue{
			fnPtr:          rawPtr,
			TotalGas:       trace.TotalGas,
			MinStack:       trace.MinStack,
			MaxStackGrowth: trace.MaxStackGrowth,
			NetStackDelta:  trace.NetStackDelta,
			NextPC:         trace.NextPC,
		}
		compiledCode := make([]byte, len(contract.Code))
		if len(jitCompiledContracts[*contract.CodeAddr]) == 0 {
			copy(compiledCode, contract.Code)
		} else {
			copy(compiledCode, jitCompiledContracts[*contract.CodeAddr])
		}
		compiledCode[startPC] = JIT_FLAG
		jitCompiledContracts[*contract.CodeAddr] = compiledCode
		fmt.Println("###", startPC, trace.NextPC)
	}
}

// [Phase 2] 실행 중 조회 (Run 루프 안에서 호출)
// 컴파일 로직 없음. 오직 조회만 수행. (매우 빠름)
func (in *EVMInterpreter) GetCachedJit(contract *Contract, pc uint64) *jitCacheValue {
	key := jitCacheKey{contract.CodeHash, pc}

	cacheLock.RLock()
	val := jitCache[key] // 없으면 nil 리턴
	cacheLock.RUnlock()

	return val
}
