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
typedef long long (*binary_op_t)(long long, long long);

long long execute_jit_func_test(void* func_ptr, long long a, long long b) {
    binary_op_t f = (binary_op_t)func_ptr;
    return f(a, b);
}



typedef void (*jit_generated_func_t)(uint64_t* stack_base, uint64_t* mem_base, uint64_t mem_len, const uint8_t* input_ptr, uint64_t input_len);

static void execute_jit_func(void* func_ptr, void* stack_base, void* mem_base, uint64_t mem_len, void* input_ptr, uint64_t input_len) {
    jit_generated_func_t jit_fn = (jit_generated_func_t)func_ptr;

    // Rust JIT 함수 호출
    jit_fn((uint64_t*)stack_base, (uint64_t*)mem_base, mem_len, (const uint8_t*)input_ptr, input_len);
}

static void test123(void* func_ptr, void* stack_base);
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
	// t := time.Now()

	// var t time.Time
	for atomic.LoadInt32(&in.evm.abort) == 0 {
		// if pc == 24 {
		// 	t = time.Now()
		// }
		// if pc == 39 {
		// 	fmt.Println("AAA", time.Since(t))
		// }

		// if pc == 234 {
		// t = time.Now()
		// }
		// if pc == 190 {
		// fmt.Println("@@@@@@@@@@@@@", time.Since(t), pc)
		// }
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
		// fmt.Println("OP", OpCode(op).String())
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

			// if contract.Address() == common.HexToAddress("0x786b81Eb450A26E8c3df0104478BA172f77003D1") {
			// 	if op == 0x52 || op == 0x53 {
			// 		fmt.Println("@@@", OpCode(op).String(), memSize, extraSize, stack.Back(0).Uint64(), mem.Len())
			// 	}
			// }
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
	// fmt.Println("ELAPSED", time.Since(t))

	abort := atomic.LoadInt32(&in.evm.abort)
	if (abort & CancelByTotalTimeLimit) != 0 {
		return nil, ErrTotalTimeLimitReached // TODO-Klaytn-Issue615
	}
	if err == errStopToken {
		err = nil // clear stop token error
	}

	// fmt.Println("INTERPRETER END", contract.CodeAddr.String())
	return res, err
}

// Run loops and evaluates the contract's code with the given input data and returns
// the return byte-slice and an error if one occurred.
//
// It's important to note that any errors returned by the interpreter should be
// considered a revert-and-consume-all-gas operation except for
// ErrExecutionReverted which means revert-and-keep-gas-left.
func (in *EVMInterpreter) JITRun(contract *Contract, input []byte) (ret []byte, err error) {
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

	// if JIT_ENGINE == nil {
	// 	JIT_ENGINE = C.new_jit_engine()
	// }
	// if HOT_FUNC == nil {
	// 	HOT_FUNC = C.compile_add_func(JIT_ENGINE)
	// }
	// fmt.Println("🚀 Running JIT code...")
	// jitResult := C.execute_jit_func_test(unsafe.Pointer(HOT_FUNC), 10, 20)
	// fmt.Printf("Result: %d\n", jitResult)

	// 	{
	// 		// if *contract.CodeAddr == common.HexToAddress("0x5dDeE20Ddf85CDc5837951653395B6015B926666") {
	// 		if *contract.CodeAddr == common.HexToAddress("0x5dDeE20Ddf85CDc5837951653395B6015B926666") {
	// 			// if *contract.CodeAddr == common.HexToAddress("0x2b807970885dE1cb66b5BD013F8E546aa2A4e7D5") {
	// 			// trajs := jit.AnalyzeTrace(contract.Code, pc)
	// 			// for pc, traj := range trajs {
	// 			// 	totalLen := uint64(0)
	// 			// 	for _, s := range traj.Segments {
	// 			// 		totalLen += s.Length
	// 			// 	}
	// 			// 	fmt.Println("LLL", pc, traj.CanJit, traj.TotalGas, totalLen)
	// 			// 	fmt.Println("@@@", pc, traj.NextPC)
	// 			// }

	// 			// for pc, traj := range trajs {
	// 			// 	for _, seg := range traj.Segments {
	// 			// 		for i := seg.Offset; i < seg.Offset+seg.Length; i++ {
	// 			// 			// fmt.Println("!!!", hexutil.Encode([]byte{contract.Code[i]}))
	// 			// 			fmt.Println("!!!", OpCode(contract.Code[i]).String(), pc, i)
	// 			// 		}
	// 			// 	}
	// 			// 	fmt.Println("----------------------------------")
	// 			// 	fmt.Println("----------------------------------")
	// 			// }

	// 			in.PrepareJit(contract)
	// 			// fmt.Println("WWW", len(jitCache))
	// 		}
	// 	}

	// 	{

	// The Interpreter main run loop (contextual). This loop runs until either an
	// explicit STOP, RETURN or SELFDESTRUCT is executed, an error occurred during
	// the execution of one of the operations or until the done flag is set by the
	// parent context.
	// t := time.Now()
	// dojit := false
	// if *contract.CodeAddr == common.HexToAddress("0x5dDeE20Ddf85CDc5837951653395B6015B926666") {
	// 	dojit = true
	// }

	// t := time.Now()
	for atomic.LoadInt32(&in.evm.abort) == 0 {
		// if pc == 165 {
		// 	t = time.Now()
		// }
		// if pc == 178 {
		// 	fmt.Println("AAA", time.Since(t))
		// }
		// if printPC {
		// 	fmt.Println("@@", pc)
		// }

		// 		nextPC, jitExecuted := in.tryExecuteJitSimple(contract, stack, pc)
		// 		if jitExecuted {
		// 			pc = nextPC
		// 			// fmt.Println("JITEXECUTED", pc)
		// 			continue
		// 		}

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
		// fmt.Println("OP", OpCode(op).String(), time.Since(t))
	}
	// fmt.Println("JIT ELAPSED", time.Since(t))
	// if *contract.CodeAddr == common.HexToAddress("0x5dDeE20Ddf85CDc5837951653395B6015B926666") {
	// 	// if *contract.CodeAddr == common.HexToAddress("0x2b807970885dE1cb66b5BD013F8E546aa2A4e7D5") {
	// fmt.Println("ELAPSED", time.Since(t))
	// 	// fmt.Println(">>", hexutil.Encode(input))
	// }
	// fmt.Println("ELAPSED", time.Since(t))

	abort := atomic.LoadInt32(&in.evm.abort)
	if (abort & CancelByTotalTimeLimit) != 0 {
		return nil, ErrTotalTimeLimitReached // TODO-Klaytn-Issue615
	}
	if err == errStopToken {
		err = nil // clear stop token error
	}

	// fmt.Println("INTERPRETER END", contract.CodeAddr.String())
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

// // tryExecuteJit: JIT 실행을 시도하고, 성공 시 다음 PC와 true를 반환합니다.
// // 실패하거나 JIT 대상이 아니면 false를 반환하여 인터프리터가 수행하게 합니다.
// func (in *EVMInterpreter) tryExecuteJit(contract *Contract, stack *Stack, pc uint64) (uint64, bool) {
// 	// t := time.Now()
// 	// 1. 엔진 초기화 (Lazy Init)
// 	in.initJitEngine()
// 	if in.jitEngine == nil {
// 		return 0, false
// 	}

// 	// 2. 캐시 조회 (Get or Compile)
// 	// getOrCompile은 내부적으로 캐시를 확인하고, 없으면 분석/컴파일 후 저장합니다.
// 	jitData := in.GetCachedJit(contract, pc)
// 	if jitData == nil {
// 		return 0, false // JIT 불가능한 코드
// 	}

// 	// 3-2: 스택 Underflow (MinStack)
// 	// 예: PUSH 10 -> POP 20 인 경우, MinStack은 10임.
// 	// 현재 스택이 10개 미만이면 실행 불가. (10개 이상이면 중간에 PUSH 덕분에 안전)
// 	if stack.len() < jitData.MinStack {
// 		return 0, false // JIT 거부 -> 인터프리터가 돌다가 에러 냄
// 	}

// 	// 3-3: 스택 Overflow (MaxStackGrowth)
// 	if stack.len()+jitData.MaxStackGrowth > int(params.StackLimit) {
// 		return 0, false
// 	}

// 	// 4. 가스 차감
// 	if !contract.UseGas(jitData.TotalGas) {
// 		return 0, false
// 	}

// 	// ---------------------------------------------------------
// 	// [실행 단계] - 검증 통과했으므로 안전하게 실행
// 	// ---------------------------------------------------------

// 	// 1. 스택 메모리 안전 확보 (Stack Growth)
// 	// JIT이 실행되면서 스택을 늘릴 때, cap이 부족하면 Crash가 나므로 미리 확보
// 	currentStackLen := stack.len()
// 	neededCap := currentStackLen + jitData.MaxStackGrowth

// 	// fmt.Println("KKKK", currentStackLen, jitData.MaxStackGrowth, neededCap, cap(stack.data))
// 	// fmt.Println("KKKK", currentStackLen, jitData.MaxStackGrowth, neededCap, cap(stack.data))
// 	// fmt.Println("KKKK", currentStackLen, jitData.MaxStackGrowth, neededCap, cap(stack.data))
// 	// fmt.Println("KKKK", currentStackLen, jitData.MaxStackGrowth, neededCap, cap(stack.data))

// 	if neededCap > cap(stack.data) {
// 		newCap := neededCap + 128 // 여유분 확보
// 		newData := make([]uint256.Int, currentStackLen, newCap)
// 		copy(newData, stack.data)
// 		stack.data = newData
// 	}

// 	// 2. JIT 실행 (Execute via CGO)
// 	// 분석이나 컴파일 과정 없이, 캐시된 '함수 포인터'를 바로 실행
// 	stackBase := unsafe.Pointer(&stack.data[0])
// 	cursorPtr := unsafe.Pointer(uintptr(stackBase) + uintptr(stack.len())*32)
// 	C.execute_jit_func(
// 		jitData.fnPtr,
// 		cursorPtr,
// 	)

// 	// 3. 상태 동기화 (State Sync)
// 	// 스택의 '데이터'는 JIT이 이미 다 바꿨음. Go에게 '길이(len)'만 알려주면 됨.
// 	newLen := currentStackLen + jitData.NetStackDelta

// 	// in.stack.data 슬라이스 길이 조정
// 	stack.data = stack.data[:newLen]

// 	// fmt.Println("TTT", time.Since(t))

// 	// 다음 실행할 PC 반환 (JIT이 처리한 블록 다음 위치)
// 	return jitData.NextPC, true
// }

func (in *EVMInterpreter) tryExecuteJitSimple(contract *Contract, stack *Stack, mem *Memory, input []byte, pc uint64) (uint64, bool) {
	// t := time.Now()
	// if pc == 224 {
	// 	t = time.Now()
	// }
	key := jitCacheKey{contract.CodeHash, pc}
	jitData := jitCache[key]
	// jitData := in.GetCachedJit(contract, pc)
	if jitData == nil {
		return 0, false // JIT 불가능한 코드
	}

	// 3-2: 스택 Underflow (MinStack)
	// 예: PUSH 10 -> POP 20 인 경우, MinStack은 10임.
	// 현재 스택이 10개 미만이면 실행 불가. (10개 이상이면 중간에 PUSH 덕분에 안전)
	if stack.len() < jitData.MinStack {
		return 0, false // JIT 거부 -> 인터프리터가 돌다가 에러 냄
	}

	// 3-3: 스택 Overflow (MaxStackGrowth)
	if stack.len()+jitData.MaxStackGrowth > int(params.StackLimit) {
		return 0, false
	}

	// 4. 가스 차감
	// constant gas
	if !contract.UseGas(jitData.TotalGas) {
		return 0, false
	}
	// dynamic gas
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

	// ---------------------------------------------------------
	// [실행 단계] - 검증 통과했으므로 안전하게 실행
	// ---------------------------------------------------------

	// 1. 스택 메모리 안전 확보 (Stack Growth)
	// JIT이 실행되면서 스택을 늘릴 때, cap이 부족하면 Crash가 나므로 미리 확보
	currentStackLen := stack.len()
	neededCap := currentStackLen + jitData.MaxStackGrowth
	if neededCap > cap(stack.data) {
		newData := make([]uint256.Int, currentStackLen, neededCap)
		copy(newData, stack.data)
		stack.data = newData
	}

	// 2. JIT 실행 (Execute via CGO)
	// 분석이나 컴파일 과정 없이, 캐시된 '함수 포인터'를 바로 실행
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
