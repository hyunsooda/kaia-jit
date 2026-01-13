package vm

import (
	"errors"
	"fmt"
	"sync"
	"unsafe"

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
void* compile_trace(void* engine, char** err_out, const uint8_t* code, const uint64_t* pc_map, size_t len);
void free_error_msg(char* s);
*/
import "C"

type jitCacheKey struct {
	codeHash common.Hash
	pc       uint64
}

type jitCacheValue struct {
	fnPtr unsafe.Pointer // Function pointer of static JIT library (written in Rust)

	TotalGas          uint64
	MinStack          int
	MaxStackGrowth    int
	NetStackDelta     int
	NetStackDeltaList []int
	MaxMemoryOff      uint64
	NextPC            uint64
}

const JIT_FLAG = byte(0xEE)

var (
	jitCache             = make(map[jitCacheKey]*jitCacheValue)
	jitCompiledContracts = make(map[common.Address][]byte)
	jitOriginContracts   = make(map[common.Address][]byte)
	analyzedContracts    = make(map[common.Hash]bool)
	cacheLock            sync.RWMutex
)

// PrepareJIT analyze the given contract source code compile and cache it if possible
func (in *EVMInterpreter) PrepareJit(contract *Contract) (bool, error) {
	codeHash := contract.CodeHash

	// 1. Fast Check: check if the contract has been analyzed
	cacheLock.RLock()
	analyzed := analyzedContracts[codeHash]
	cacheLock.RUnlock()
	if analyzed {
		return false, nil
	}

	// 2. Analysis: Find compilable code blocks
	batchResults, err := AnalyzeTrace(contract.Code, 0)
	if err != nil {
		return false, err
	}
	analyzedContracts[codeHash] = true
	if len(batchResults) == 0 {
		return false, nil
	}

	// 3. Initialize JIT engine if not intialized before
	in.initJitEngine()
	if in.jitEngine == nil {
		return false, errors.New("JIT engine is not initialized")
	}

	// 4. Compile the analyzed code blocks which are legal to compile
	cacheLock.Lock()
	defer cacheLock.Unlock()

	for startPC, trace := range batchResults {
		key := jitCacheKey{codeHash, startPC}

		if _, ok := jitCache[key]; ok {
			continue
		}

		// Stitching
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

		if len(linearCode) == 0 {
			continue
		}

		var cErrMsg *C.char = nil
		// Compile JIT
		rawPtr := C.compile_trace(
			in.jitEngine,
			(**C.char)(unsafe.Pointer(&cErrMsg)),
			(*C.uint8_t)(unsafe.Pointer(&linearCode[0])),
			(*C.uint64_t)(unsafe.Pointer(&linearPC[0])),
			C.size_t(len(linearCode)),
		)
		if cErrMsg != nil {
			goMsg := C.GoString(cErrMsg)
			C.free_error_msg(cErrMsg)
			return false, errors.New(fmt.Sprintf("JIT compilation error: %s", goMsg))
		}
		if rawPtr == nil {
			return false, errors.New("JIT compilation failed with unknown error")
		}

		// Cache the compiled code into the cache
		jitCache[key] = &jitCacheValue{
			fnPtr:             rawPtr,
			TotalGas:          trace.TotalGas,
			MinStack:          trace.MinStack,
			MaxStackGrowth:    trace.MaxStackGrowth,
			MaxMemoryOff:      trace.MaxMemoryOff,
			NetStackDelta:     trace.NetStackDelta,
			NetStackDeltaList: trace.NetStackDeltaList,
			NextPC:            trace.NextPC,
		}
		compiledCode := make([]byte, len(contract.Code))
		if len(jitCompiledContracts[*contract.CodeAddr]) == 0 {
			copy(compiledCode, contract.Code)
		} else {
			copy(compiledCode, jitCompiledContracts[*contract.CodeAddr])
		}
		compiledCode[startPC] = JIT_FLAG
		jitCompiledContracts[*contract.CodeAddr] = compiledCode
		// fmt.Printf("### %x %x (%x)\n", startPC, trace.NextPC, trace.MaxMemoryOff)
	}
	if jitCompiledContracts[*contract.CodeAddr] != nil {
		jitOriginContracts[*contract.CodeAddr] = contract.Code
	}
	return true, nil
}

func (in *EVMInterpreter) GetCachedJit(contract *Contract, pc uint64) *jitCacheValue {
	key := jitCacheKey{contract.CodeHash, pc}
	cacheLock.RLock()
	val := jitCache[key]
	cacheLock.RUnlock()
	return val
}
