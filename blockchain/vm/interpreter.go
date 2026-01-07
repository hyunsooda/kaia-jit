// Modifications Copyright 2024 The Kaia Authors
// Modifications Copyright 2018 The klaytn Authors
// Copyright 2015 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.
//
// This file is derived from core/vm/interpreter.go (2018/06/04).
// Modified and improved for the klaytn development.
// Modified and improved for the Kaia development.

package vm

/*
#cgo LDFLAGS: ${SRCDIR}/../../kaiax/jit_engine/target/release/libjit_engine.a -ldl -lm -lpthread
#include <stdlib.h>
#include <stdint.h>

void* new_jit_engine();
uint8_t* compile_add_func(void* engine);
void free_jit_engine(void* engine);

// Trampoline
typedef void (*jit_generated_func_t)(uint64_t* stack_base, uint64_t* mem_base, uint64_t mem_len, const uint8_t* input_ptr, uint64_t input_len);

static void execute_jit_func(void* func_ptr, void* stack_base, void* mem_base, uint64_t mem_len, void* input_ptr, uint64_t input_len) {
    jit_generated_func_t jit_fn = (jit_generated_func_t)func_ptr;

    // Rust JIT 함수 호출
    jit_fn((uint64_t*)stack_base, (uint64_t*)mem_base, mem_len, (const uint8_t*)input_ptr, input_len);
}

*/
import "C"

import (
	"errors"
	"fmt"
	"hash"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/holiman/uint256"
	"github.com/kaiachain/kaia/blockchain/vm/jitcall"
	"github.com/kaiachain/kaia/common"
	"github.com/kaiachain/kaia/common/math"
	"github.com/kaiachain/kaia/kerrors"
	"github.com/kaiachain/kaia/params"
)

var (
	JIT_ENGINE unsafe.Pointer
	HOT_FUNC   *C.uint8_t

	printPC = false
)

func PrintPC() {
	printPC = true
}

// Config are the configuration options for the Interpreter
type Config struct {
	Debug                   bool   // Enables debugging
	Tracer                  Tracer // Opcode logger
	NoRecursion             bool   // Disables call, callcode, delegate call and create
	EnablePreimageRecording bool   // Enables recording of SHA3/keccak preimages

	JumpTable JumpTable // EVM instruction table, automatically populated if unset

	// RunningEVM is to indicate the running EVM and used to stop the EVM.
	RunningEVM chan *EVM

	// ComputationCostLimit is the limit of the total computation cost of a transaction. Set infinite to disable the computation cost limit.
	ComputationCostLimit uint64

	// UseConsoleLog enables console.log() in solidity for local network
	UseConsoleLog bool

	// Enables collecting internal transaction data during processing a block
	EnableInternalTxTracing bool

	// Enables collecting and printing opcode execution time
	EnableOpDebug bool

	// Prefetching is true if the EVM is used for prefetching.
	Prefetching bool

	// Additional EIPs that are to be enabled
	ExtraEips []int
}

// ScopeContext contains the things that are per-call, such as stack and memory,
// but not transients like pc and gas
type ScopeContext struct {
	Memory   *Memory
	Stack    *Stack
	Contract *Contract
}

// keccakState wraps sha3.state. In addition to the usual hash methods, it also supports
// Read to get a variable amount of data from the hash state. Read is faster than Sum
// because it doesn't copy the internal state, but also modifies the internal state.
type keccakState interface {
	hash.Hash
	Read([]byte) (int, error)
}

// EVMInterpreter is used to run Kaia based contracts and will utilise the
// passed environment to query external sources for state information.
// The EVMInterpreter will run the byte code VM based on the passed
// configuration.
type EVMInterpreter struct {
	evm *EVM
	cfg *Config

	hasher    keccakState // Keccak256 hasher instance shared across opcodes
	hasherBuf common.Hash // Keccak256 hasher result array shared aross opcodes

	readOnly   bool   // Whether to throw on stateful modifications
	returnData []byte // Last CALL's return data for subsequent reuse

	jitEngine unsafe.Pointer // Rust JIT 엔진 포인터 (void*)
	jitOnce   sync.Once      // 스레드 안전한 초기화 보장
}

func (in *EVMInterpreter) GetJumpTbl() (JumpTable, error) {
	if in.cfg != nil {
		return in.cfg.JumpTable, nil
	}
	return JumpTable{}, errors.New("no jump table exist")
}

// initJitEngine: 최초 1회만 실행됨
func (in *EVMInterpreter) initJitEngine() {
	in.jitOnce.Do(func() {
		// Rust의 new_jit_engine 호출
		in.jitEngine = C.new_jit_engine()
	})
}

// NewEVMInterpreter returns a new instance of the Interpreter.
func NewEVMInterpreter(evm *EVM) *EVMInterpreter {
	// We use the STOP instruction whether to see
	// the jump table was initialised. If it was not
	// we'll set the default jump table.
	cfg := evm.Config
	if cfg.JumpTable[STOP] == nil {
		var jt JumpTable
		switch {
		case evm.chainRules.IsOsaka:
			jt = OsakaInstructionSet
		case evm.chainRules.IsPrague:
			jt = PragueInstructionSet
		case evm.chainRules.IsCancun:
			jt = CancunInstructionSet
		case evm.chainRules.IsShanghai:
			jt = ShanghaiInstructionSet
		case evm.chainRules.IsKore:
			jt = KoreInstructionSet
		case evm.chainRules.IsLondon:
			jt = LondonInstructionSet
		case evm.chainRules.IsIstanbul:
			jt = IstanbulInstructionSet
		default:
			jt = ConstantinopleInstructionSet
		}

		// TODO: Make configurable by node CLI
		enableJitFlag(&jt)

		for i, eip := range cfg.ExtraEips {
			if err := EnableEIP(eip, &jt); err != nil {
				// Disable it, so caller can check if it's activated or not
				cfg.ExtraEips = append(cfg.ExtraEips[:i], cfg.ExtraEips[i+1:]...)
				logger.Error("EIP activation failed", "eip", eip, "error", err)
			}
		}
		cfg.JumpTable = jt
	}

	// When setting computation cost limit value, priority is given to the original value, override experimental value,
	// and then the limit value specified for each hard fork. If the original value is not infinite or
	// there is no override value, the next priority value is used.
	// Cautious, the infinite value is only applicable for specific API calls. (e.g. call/estimateGas/estimateComputationGas)
	if cfg.ComputationCostLimit == params.OpcodeComputationCostLimitInfinite {
		return &EVMInterpreter{evm: evm, cfg: cfg}
	}
	// Override the computation cost with an experiment value
	if params.OpcodeComputationCostLimitOverride != 0 {
		cfg.ComputationCostLimit = params.OpcodeComputationCostLimitOverride
		return &EVMInterpreter{evm: evm, cfg: cfg}
	}
	// Set the opcode computation cost limit by the default value
	switch {
	case evm.chainRules.IsCancun:
		cfg.ComputationCostLimit = uint64(params.OpcodeComputationCostLimitCancun)
	default:
		cfg.ComputationCostLimit = uint64(params.OpcodeComputationCostLimit)
	}
	return &EVMInterpreter{evm: evm, cfg: cfg}
}

// count values and execution time of the opcodes are collected until the node is turned off.
var (
	opCnt  = make([]uint64, 256)
	opTime = make([]uint64, 256)
)

func (in *EVMInterpreter) Run(contract *Contract, input []byte) (ret []byte, err error) {
	// Increment the call depth which is restricted to 1024
	in.evm.depth++
	defer func() { in.evm.depth-- }()

	// Reset the previous call's return data. It's unimportant to preserve the old buffer
	// as every returning call will return new data anyway.
	in.returnData = nil

	// Don't bother with the execution if there's no code.
	if len(contract.Code) == 0 {
		return nil, nil
	}

	var (
		op          OpCode        // current opcode
		mem         = NewMemory() // bound memory
		stack       = newstack()  // local stack
		callContext = &ScopeContext{
			Memory:   mem,
			Stack:    stack,
			Contract: contract,
		}
		// For optimisation reason we're using uint64 as the program counter.
		// It's theoretically possible to go above 2^64. The YP defines the PC
		// to be uint256. Practically much less so feasible.
		pc       = uint64(0) // program counter
		cost     uint64
		ccOpcode uint64
		// copies used by tracer
		pcCopy              uint64 // needed for the deferred Tracer
		gasCopy             uint64 // for Tracer to log gas remaining before execution
		logged              bool   // deferred Tracer should ignore already logged steps
		ccLeftCopy          uint64
		res                 []byte              // result of the opcode execution function
		allocatedMemorySize = uint64(mem.Len()) // Currently allocated memory size

		// used for collecting opcode execution time
		opExecStart time.Time
	)
	contract.Input = input

	if in.cfg.Debug {
		defer func() {
			if err != nil {
				if !logged {
					in.cfg.Tracer.CaptureState(in.evm, pcCopy, op, gasCopy, cost, ccLeftCopy, ccOpcode, callContext, in.evm.depth, err)
				} else {
					in.cfg.Tracer.CaptureFault(in.evm, pcCopy, op, gasCopy, cost, ccLeftCopy, ccOpcode, callContext, in.evm.depth, err)
				}
			}
		}()
	}

	// The Interpreter main run loop (contextual). This loop runs until either an
	// explicit STOP, RETURN or SELFDESTRUCT is executed, an error occurred during
	// the execution of one of the operations or until the done flag is set by the
	// parent context.
	for atomic.LoadInt32(&in.evm.abort) == 0 {
		if in.evm.Config.EnableOpDebug {
			opExecStart = time.Now()
		}
		if in.cfg.Debug {
			// Capture pre-execution values for tracing.
			logged, pcCopy, gasCopy, ccLeftCopy = false, pc, contract.Gas, in.evm.Config.ComputationCostLimit-in.evm.opcodeComputationCostSum
		}

		// Get the operation from the jump table and validate the stack to ensure there are
		// enough stack items available to perform the operation.
		op = contract.GetOp(pc)
		operation := in.cfg.JumpTable[op]
		if operation == nil {
			return nil, fmt.Errorf("invalid opcode 0x%x", int(op)) // TODO-Klaytn-Issue615
		}
		// Validate stack
		if sLen := stack.len(); sLen < operation.minStack {
			return nil, fmt.Errorf("stack underflow (%d <=> %d)", sLen, operation.minStack)
		} else if sLen > operation.maxStack {
			return nil, fmt.Errorf("stack limit reached %d (%d)", sLen, operation.maxStack)
		}

		// Static portion of gas
		cost = operation.constantGas // For tracing
		if !contract.UseGas(operation.constantGas) {
			return nil, kerrors.ErrOutOfGas
		}

		// We limit tx's execution time using the sum of computation cost of opcodes.
		in.evm.opcodeComputationCostSum += operation.computationCost
		if in.evm.opcodeComputationCostSum > in.evm.Config.ComputationCostLimit {
			return nil, ErrOpcodeComputationCostLimitReached
		}
		ccOpcode = operation.computationCost
		var memorySize uint64
		var extraSize uint64
		// calculate the new memory size and expand the memory to fit
		// the operation
		// Memory check needs to be done prior to evaluating the dynamic gas portion,
		// to detect calculation overflows
		if operation.memorySize != nil {
			memSize, overflow := operation.memorySize(stack)
			if overflow {
				return nil, errGasUintOverflow // TODO-Klaytn-Issue615
			}
			// memory is expanded in words of 32 bytes. Gas
			// is also calculated in words.
			if memorySize, overflow = math.SafeMul(toWordSize(memSize), 32); overflow {
				return nil, errGasUintOverflow // TODO-Klaytn-Issue615
			}
			if allocatedMemorySize < memorySize {
				extraSize = memorySize - allocatedMemorySize
			}
		}
		// Dynamic portion of gas
		// consume the gas and return an error if not enough gas is available.
		// cost is explicitly set so that the capture state defer method can get the proper cost
		if operation.dynamicGas != nil {
			var dynamicCost uint64
			dynamicCost, err = operation.dynamicGas(in.evm, contract, stack, mem, memorySize)
			cost += dynamicCost // total cost, for debug tracing
			if err != nil || !contract.UseGas(dynamicCost) {
				return nil, kerrors.ErrOutOfGas // TODO-Klaytn-Issue615
			}
		}
		if extraSize > 0 {
			mem.Increase(extraSize)
			allocatedMemorySize = uint64(mem.Len())
		}

		if in.cfg.Debug {
			in.cfg.Tracer.CaptureState(in.evm, pc, op, gasCopy, cost, ccLeftCopy, ccOpcode, callContext, in.evm.depth, err)
			logged = true
		}

		// execute the operation
		res, err = operation.execute(&pc, in, &ScopeContext{mem, stack, contract})
		if in.evm.Config.EnableOpDebug {
			opTime[op] += uint64(time.Since(opExecStart).Nanoseconds())
			opCnt[op] += 1
		}
		if err != nil {
			break
		}
		pc++
	}

	abort := atomic.LoadInt32(&in.evm.abort)
	if (abort & CancelByTotalTimeLimit) != 0 {
		return nil, ErrTotalTimeLimitReached // TODO-Klaytn-Issue615
	}
	if err == errStopToken {
		err = nil // clear stop token error
	}
	return res, err
}

func PrintOpCodeExecTime() {
	logger.Info("Printing the execution time of the opcodes during this node operation")
	for i := 0; i < 256; i++ {
		if opCnt[i] > 0 {
			logger.Info("op "+OpCode(i).String(), "cnt", opCnt[i], "avg", opTime[i]/opCnt[i])
		}
	}
}

func (in *EVMInterpreter) IsJitEngineInitialized() bool {
	return in.jitEngine != nil
}

func (in *EVMInterpreter) GetJitEngine() unsafe.Pointer {
	return in.jitEngine
}

func (in *EVMInterpreter) ExecuteJIT(contract *Contract, stack *Stack, mem *Memory, input []byte, pc uint64) (uint64, bool) {
	key := jitCacheKey{contract.CodeHash, pc}
	jitData := jitCache[key]
	if jitData == nil {
		return 0, false
	}

	if stack.len() < jitData.MinStack {
		// Stack validation: underflow
		return 0, false // JIT 거부 -> 인터프리터가 돌다가 에러 냄
	}

	if stack.len()+jitData.MaxStackGrowth > int(params.StackLimit) {
		// Stack validation: overflow
		return 0, false
	}

	// Gas consumption: constant gas
	if !contract.UseGas(jitData.TotalGas) {
		return 0, false
	}
	// Gas consumption: dynamic gas
	if jitData.MaxMemoryOff > uint64(mem.Len()) {
		mem.Resize(jitData.MaxMemoryOff)
		dynamicCost, err := memoryGasCost(mem, jitData.MaxMemoryOff)
		if err != nil {
			// TODO: add log error
			return 0, false
		}
		if !contract.UseGas(dynamicCost) {
			// TODO: add log error
			return 0, false
		}
	}

	// Allocate stack space if needs
	currentStackLen := stack.len()
	neededCap := currentStackLen + jitData.MaxStackGrowth
	if neededCap > cap(stack.data) {
		newData := make([]uint256.Int, currentStackLen, neededCap)
		copy(newData, stack.data)
		stack.data = newData
	}

	// Execute via CGO
	var (
		stackBase                = unsafe.Pointer(unsafe.SliceData(stack.data))
		cursorPtr                = unsafe.Pointer(uintptr(stackBase) + uintptr(stack.len())*32)
		inputPtr  unsafe.Pointer = nil
		inputLen                 = uint64(len(input))
		memPtr    unsafe.Pointer = nil
		memData                  = mem.Data()
		memLen                   = uint64(mem.Len())
	)
	if len(input) > 0 {
		inputPtr = unsafe.Pointer(&input[0])
	}
	if len(memData) > 0 {
		memPtr = unsafe.Pointer(&memData[0])
	}

	// t := time.Now()
	jitcall.Execute(
		jitData.fnPtr,
		cursorPtr,
		memPtr,
		memLen,
		inputPtr,
		inputLen,
	)

	// fmt.Println("TT", time.Since(t), jitData.NextPC)
	// C.execute_jit_func(
	// 	jitData.fnPtr,
	// 	cursorPtr,
	// 	memPtr,
	// 	C.uint64_t(memLen),
	// 	inputPtr,
	// 	C.uint64_t(inputLen),
	// )
	newLen := currentStackLen + jitData.NetStackDelta
	stack.data = stack.data[:newLen]
	return jitData.NextPC, true
}
