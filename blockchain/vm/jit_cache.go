package vm

import (
	"sync"
	"unsafe"

	"github.com/kaiachain/kaia/blockchain/vm/jit"
	"github.com/kaiachain/kaia/common"
)

/*
// [JIT 컴파일 함수 선언]
// Input:
//   - engine: Rust JIT 엔진 포인터 (void*)
//   - code:   합쳐진(Stitched) 바이트코드 배열 포인터
//   - len:    바이트코드 길이
// Output:
//   - 실행 가능한 기계어 함수의 메모리 주소 (void*)
void* compile_trajactory(void* engine, unsigned char* code, size_t len);
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

var (
	// 포인터를 저장하도록 변경 (*jitCacheValue)
	jitCache  = make(map[jitCacheKey]*jitCacheValue)
	cacheLock sync.RWMutex
)

// getOrCompile: 캐시를 조회하거나, 없으면 컴파일해서 저장 후 반환
// 리턴값: (*jitCacheValue, bool) -> bool이 false면 JIT 불가
func (in *EVMInterpreter) getOrCompile(contract *Contract, pc uint64) (*jitCacheValue, bool) {
	key := jitCacheKey{
		codeHash: contract.CodeHash,
		pc:       pc,
	}

	// 1. [Fast Path] 캐시 조회 (Read Lock)
	cacheLock.RLock()
	if val, ok := jitCache[key]; ok {
		cacheLock.RUnlock()
		return val, true
	}
	cacheLock.RUnlock()

	// 2. [Analyze] 일괄 분석 (Batch Analysis)
	// trajectory는 map[uint64]JitTraceResult 입니다.
	// 분석기는 pc부터 시작해서 가능한 모든 JIT 블록을 찾아냅니다.
	trajectories := jit.AnalyzeTrace(contract.Code, pc)

	// 우리가 요청한 pc에 해당하는 블록이 없으면 JIT 불가
	// (뒤쪽에 다른 블록이 발견됐을 순 있지만, 당장 실행할 건 없음)
	if _, ok := trajectories[pc]; !ok {
		return nil, false
	}

	// 3. 엔진 체크
	in.initJitEngine()
	if in.jitEngine == nil {
		logger.Error("JIT Engine has not been initialized")
		return nil, false
	}

	// 4. [Bulk Save] 찾아낸 모든 Trace를 컴파일하고 저장 (Write Lock)
	cacheLock.Lock()
	defer cacheLock.Unlock()

	// Double Check: 락 대기 중에 다른 스레드가 이미 만들었는지 확인
	if val, ok := jitCache[key]; ok {
		return val, true
	}

	// 맵 전체를 돌면서 "발견된 모든 블록"을 다 캐싱합니다.
	for startPC, traj := range trajectories {

		// 4-1. 코드 스티칭 (Stitching)
		totalLen := 0
		for _, seg := range traj.Segments {
			totalLen += int(seg.Length)
		}

		linearCode := make([]byte, 0, totalLen)
		for _, seg := range traj.Segments {
			linearCode = append(linearCode, contract.Code[seg.Offset:seg.Offset+seg.Length]...)
		}

		// 4-2. 컴파일 (Rust 호출)
		rawPtr := C.compile_trajactory(
			in.jitEngine, // in.GetJitEngine() 등으로 접근 필요할 수 있음
			(*C.uchar)(unsafe.Pointer(&linearCode[0])),
			C.size_t(len(linearCode)),
		)

		if rawPtr == nil {
			continue // 컴파일 실패 시 해당 블록만 스킵
		}

		// 4-3. 캐시 값 생성
		newValue := &jitCacheValue{
			fnPtr:          rawPtr,
			TotalGas:       traj.TotalGas,
			MinStack:       traj.MinStack,
			MaxStackGrowth: traj.MaxStackGrowth,
			NetStackDelta:  traj.NetStackDelta,
			NextPC:         traj.NextPC,
		}

		// 4-4. 전역 캐시에 등록
		// startPC를 Key로 사용
		cacheKey := jitCacheKey{contract.CodeHash, startPC}
		jitCache[cacheKey] = newValue
	}
	return nil, false
}
