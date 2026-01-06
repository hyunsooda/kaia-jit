use crate::engine::JitEngine;
use crate::inline::InlineOps;
use cranelift::{codegen::verify_function, prelude::*};
use cranelift_jit::JITModule;
use cranelift_module::{FuncId, Linkage, Module};
use std::collections::HashMap;
use std::ffi::CString;
use std::os::raw::c_char;
use std::ptr;
use std::slice;

#[no_mangle]
pub extern "C" fn compile_trace(
    ptr: *mut JitEngine,
    err_out: *mut *mut c_char,
    code_ptr: *const u8,
    pc_map_ptr: *const u64,
    len: usize,
) -> *const u8 {
    let engine = unsafe { &mut *ptr };
    let ctx = &mut engine.ctx;
    let module = &mut engine.module;
    let funcs = &mut engine.funcs;

    let bytecode = unsafe { slice::from_raw_parts(code_ptr, len) };
    let pc_map = unsafe { slice::from_raw_parts(pc_map_ptr, len) };

    module.clear_context(ctx);

    // Function Signature: fn(stack_cursor: *mut u64)
    let ptr_type = module.target_config().pointer_type();
    ctx.func.signature.params.push(AbiParam::new(ptr_type)); // 0: stack_cursor
    ctx.func.signature.params.push(AbiParam::new(ptr_type)); // 1: memory_ptr
    ctx.func.signature.params.push(AbiParam::new(types::I64)); // 2: memory_len [NEW!]
    ctx.func.signature.params.push(AbiParam::new(ptr_type)); // 3: input_ptr
    ctx.func.signature.params.push(AbiParam::new(types::I64)); // 4: input_len

    {
        let mut builder = FunctionBuilder::new(&mut ctx.func, &mut engine.builder_context);
        let entry = builder.create_block();
        builder.append_block_params_for_function_params(entry);
        builder.switch_to_block(entry);

        let params = builder.block_params(entry);

        let stack_cursor = params[0];
        let memory_ptr = params[1];
        let memory_len = params[2]; // [NEW!] 현재 메모리 크기 (u64)
        let input_ptr = params[3];
        let input_len = params[4];

        // --- Load/Store Helpers ---
        // 1. i64 x 4 Load
        let load_i64_x4 = |b: &mut FunctionBuilder, off: i32| -> [Value; 4] {
            let mem = MemFlags::new();
            let mut val = [b.ins().iconst(types::I64, 0); 4];
            for i in 0..4 {
                let ptr = b.ins().iadd_imm(stack_cursor, (off + i * 8) as i64);
                val[i as usize] = b.ins().load(types::I64, mem, ptr, 0);
            }
            val
        };

        // 2. i64 x 4 Store
        let store_i64_x4 = |b: &mut FunctionBuilder, off: i32, val: [Value; 4]| {
            let mem = MemFlags::new();
            for i in 0..4 {
                let ptr = b.ins().iadd_imm(stack_cursor, (off + i * 8) as i64);
                b.ins().store(mem, val[i as usize], ptr, 0);
            }
        };

        // 3. i128 x 2 Load
        let load_i128_x2 = |b: &mut FunctionBuilder, off: i32| -> [Value; 2] {
            let mem = MemFlags::new();
            let ptr_lo = b.ins().iadd_imm(stack_cursor, off as i64);
            let ptr_hi = b.ins().iadd_imm(stack_cursor, (off + 16) as i64);
            [
                b.ins().load(types::I128, mem, ptr_lo, 0),
                b.ins().load(types::I128, mem, ptr_hi, 0),
            ]
        };

        // 4. i128 x 2 Store
        let store_i128_x2 = |b: &mut FunctionBuilder, off: i32, val: [Value; 2]| {
            let mem = MemFlags::new();
            let ptr_lo = b.ins().iadd_imm(stack_cursor, off as i64);
            let ptr_hi = b.ins().iadd_imm(stack_cursor, (off + 16) as i64);
            b.ins().store(mem, val[0], ptr_lo, 0);
            b.ins().store(mem, val[1], ptr_hi, 0);
        };

        // Code segment traversal (Cursor, not PC)
        let mut cursor = 0;
        let mut offset: i32 = 0;

        while cursor < bytecode.len() {
            let op = bytecode[cursor];
            let current_real_pc = pc_map[cursor];
            cursor += 1;

            match op {
                // -------------------------------------------------------------
                // [Inline] Arithmetic (ADD, SUB) -> i64 x 4
                // -------------------------------------------------------------
                0x01 => {
                    // ADD
                    let top = load_i64_x4(&mut builder, offset - 32);
                    let second = load_i64_x4(&mut builder, offset - 64);
                    let res = InlineOps::u256_add(&mut builder, top, second);
                    store_i64_x4(&mut builder, offset - 64, res);
                    offset -= 32;
                }
                0x03 => {
                    // SUB (EVM: a - b, stack: [a, b(top)])
                    let top = load_i64_x4(&mut builder, offset - 32);
                    let second = load_i64_x4(&mut builder, offset - 64);
                    let res = InlineOps::u256_sub(&mut builder, top, second);
                    store_i64_x4(&mut builder, offset - 64, res);
                    offset -= 32;
                }

                // -------------------------------------------------------------
                // [Inline] Bitwise (AND, OR, XOR, NOT) -> i128 x 2
                // -------------------------------------------------------------
                0x16 | 0x17 | 0x18 => {
                    // AND, OR, XOR (Commutative)
                    let second = load_i128_x2(&mut builder, offset - 64);
                    let top = load_i128_x2(&mut builder, offset - 32);

                    let op_func = match op {
                        0x16 => |b: &mut FunctionBuilder, x, y| b.ins().band(x, y),
                        0x17 => |b: &mut FunctionBuilder, x, y| b.ins().bor(x, y),
                        0x18 => |b: &mut FunctionBuilder, x, y| b.ins().bxor(x, y),
                        _ => unreachable!(),
                    };

                    let res = InlineOps::u256_bitwise_128(&mut builder, top, second, op_func);
                    store_i128_x2(&mut builder, offset - 64, res);
                    offset -= 32;
                }
                0x19 => {
                    // NOT (Unary)
                    let top = load_i128_x2(&mut builder, offset - 32);
                    let res = InlineOps::u256_not_128(&mut builder, top);
                    store_i128_x2(&mut builder, offset - 32, res); // In-place
                }

                // -------------------------------------------------------------
                // [Inline] Comparison
                // -------------------------------------------------------------
                0x10 => {
                    // LT (Top < Second)
                    let second = load_i64_x4(&mut builder, offset - 64);
                    let top = load_i64_x4(&mut builder, offset - 32);

                    let res =
                        InlineOps::u256_cmp(&mut builder, top, second, IntCC::UnsignedLessThan);
                    store_i64_x4(&mut builder, offset - 64, res);
                    offset -= 32;
                }

                0x11 => {
                    // GT (Top > Second)
                    let second = load_i64_x4(&mut builder, offset - 64);
                    let top = load_i64_x4(&mut builder, offset - 32);

                    // [수정] Top > Second
                    let res =
                        InlineOps::u256_cmp(&mut builder, top, second, IntCC::UnsignedGreaterThan);
                    store_i64_x4(&mut builder, offset - 64, res);
                    offset -= 32;
                }

                0x12 => {
                    // SLT (Top < Second Signed)
                    let second = load_i64_x4(&mut builder, offset - 64);
                    let top = load_i64_x4(&mut builder, offset - 32);

                    // [수정] Top < Second (Signed)
                    let res = InlineOps::u256_cmp(&mut builder, top, second, IntCC::SignedLessThan);
                    store_i64_x4(&mut builder, offset - 64, res);
                    offset -= 32;
                }

                0x13 => {
                    // SGT (Top > Second Signed)
                    let second = load_i64_x4(&mut builder, offset - 64);
                    let top = load_i64_x4(&mut builder, offset - 32);

                    // [수정] Top > Second (Signed)
                    let res =
                        InlineOps::u256_cmp(&mut builder, top, second, IntCC::SignedGreaterThan);
                    store_i64_x4(&mut builder, offset - 64, res);
                    offset -= 32;
                }

                0x14 => {
                    // EQ (Commutative)
                    let second = load_i64_x4(&mut builder, offset - 64);
                    let top = load_i64_x4(&mut builder, offset - 32);
                    let res = InlineOps::u256_eq(&mut builder, top, second);
                    store_i64_x4(&mut builder, offset - 64, res);
                    offset -= 32;
                }

                0x15 => {
                    // ISZERO
                    let a = load_i64_x4(&mut builder, offset - 32);
                    let res = InlineOps::u256_iszero(&mut builder, a);
                    store_i64_x4(&mut builder, offset - 32, res);
                }

                0x20 => {
                    let fid = get_runtime_func(module, funcs, "jit_sha3", 3);
                    let ptr_size_dest = builder.ins().iadd_imm(stack_cursor, (offset - 64) as i64);
                    let ptr_offset = builder.ins().iadd_imm(stack_cursor, (offset - 32) as i64);
                    let func_ref = module.declare_func_in_func(fid, builder.func);
                    builder
                        .ins()
                        .call(func_ref, &[ptr_size_dest, ptr_offset, memory_ptr]);
                    offset -= 32;
                }

                // SHL (0x1b)
                0x1b => {
                    let val = load_i64_x4(&mut builder, offset - 64); // Stack[Top-1] (Value)
                    let shift = load_i64_x4(&mut builder, offset - 32); // Stack[Top] (Shift)

                    // EVM: SHL(shift, value)
                    let res = InlineOps::u256_shl(&mut builder, shift, val);

                    store_i64_x4(&mut builder, offset - 64, res);
                    offset -= 32;
                }

                // SHR (0x1c)
                0x1c => {
                    let val = load_i64_x4(&mut builder, offset - 64);
                    let shift = load_i64_x4(&mut builder, offset - 32);

                    // EVM: SHR(shift, value)
                    let res = InlineOps::u256_shr(&mut builder, shift, val);

                    store_i64_x4(&mut builder, offset - 64, res);
                    offset -= 32;
                }

                // SAR (0x1d)
                0x1d => {
                    let val = load_i64_x4(&mut builder, offset - 64);
                    let shift = load_i64_x4(&mut builder, offset - 32);

                    // EVM: SAR(shift, value)
                    let res = InlineOps::u256_sar(&mut builder, shift, val);

                    store_i64_x4(&mut builder, offset - 64, res);
                    offset -= 32;
                }

                // -------------------------------------------------------------
                // [Host Call] Complex Ops (DIV, MOD, EXP, etc)
                // -------------------------------------------------------------
                // Binary Ops
                0x02 | 0x04..=0x07 | 0x0a | 0x0b | 0x1a..=0x1d => {
                    let func_name = match op {
                        0x02 => "jit_mul",
                        0x04 => "jit_div",
                        0x05 => "jit_sdiv",
                        0x06 => "jit_mod",
                        0x07 => "jit_smod",
                        0x0b => "jit_signextend",
                        0x1a => "jit_byte",
                        // 0x1b => "jit_shl",
                        // 0x1c => "jit_shr",
                        // 0x1d => "jit_sar",
                        _ => unreachable!(),
                    };

                    let fid = get_runtime_func(module, funcs, func_name, 2);

                    // a=Top-1(Dest), b=Top(Src)
                    let ptr_dest = builder.ins().iadd_imm(stack_cursor, (offset - 64) as i64);
                    let ptr_src = builder.ins().iadd_imm(stack_cursor, (offset - 32) as i64);

                    let func_ref = module.declare_func_in_func(fid, builder.func);
                    builder.ins().call(func_ref, &[ptr_dest, ptr_src]);

                    offset -= 32;
                }

                // Ternary Ops
                0x08 | 0x09 => {
                    let func_name = if op == 0x08 {
                        "jit_addmod"
                    } else {
                        "jit_mulmod"
                    };
                    let fid = get_runtime_func(module, funcs, func_name, 3);

                    let ptr_dest = builder.ins().iadd_imm(stack_cursor, (offset - 96) as i64);
                    let ptr_mid = builder.ins().iadd_imm(stack_cursor, (offset - 64) as i64);
                    let ptr_top = builder.ins().iadd_imm(stack_cursor, (offset - 32) as i64);

                    let func_ref = module.declare_func_in_func(fid, builder.func);
                    builder.ins().call(func_ref, &[ptr_dest, ptr_mid, ptr_top]);

                    offset -= 64;
                }

                // -------------------------------------------------------------
                // [Environment Operations]
                // -------------------------------------------------------------

                // CALLDATALOAD (0x35)
                // 0x35 => {
                //     let fid = get_runtime_func(module, funcs, "jit_calldataload", 3);
                //     let func_ref = module.declare_func_in_func(fid, builder.func);
                //     let ptr_top = builder.ins().iadd_imm(stack_cursor, (offset - 32) as i64);
                //     builder
                //         .ins()
                //         .call(func_ref, &[ptr_top, input_ptr, input_len]);
                // }

                // CALLDATALOAD (0x35) [Stability Fix: Revert to I64 to prevent Alignment Crash]
                0x35 => {
                    let ptr_top = builder.ins().iadd_imm(stack_cursor, (offset - 32) as i64);
                    // [중요] unaligned 접근을 명시하지만, I128은 CPU 레벨에서 터질 수 있어 I64가 안전함
                    let mem = MemFlags::new();

                    // --- [1] 조건 검사 (Checks) ---
                    let evm_offset = builder.ins().load(types::I64, mem, ptr_top, 0);

                    // High Bits Check
                    let w1 = builder.ins().load(types::I64, mem, ptr_top, 8);
                    let w2 = builder.ins().load(types::I64, mem, ptr_top, 16);
                    let w3 = builder.ins().load(types::I64, mem, ptr_top, 24);

                    let high_tmp = builder.ins().bor(w1, w2);
                    let high_bits = builder.ins().bor(high_tmp, w3);

                    // Blocks
                    let block_check_range = builder.create_block();
                    let block_check_end = builder.create_block();
                    let block_fast = builder.create_block();
                    let block_zero = builder.create_block();
                    let block_partial = builder.create_block();
                    let block_done = builder.create_block();

                    let zero = builder.ins().iconst(types::I64, 0);

                    // (1) Huge Offset -> Zero
                    let has_high_bits = builder.ins().icmp_imm(IntCC::NotEqual, high_bits, 0);
                    builder
                        .ins()
                        .brif(has_high_bits, block_zero, &[], block_check_range, &[]);

                    // (2) Total OOB -> Zero
                    builder.switch_to_block(block_check_range);
                    let is_total_oob = builder.ins().icmp(
                        IntCC::UnsignedGreaterThanOrEqual,
                        evm_offset,
                        input_len,
                    );

                    builder
                        .ins()
                        .brif(is_total_oob, block_zero, &[], block_check_end, &[]);

                    // (3) Partial Check
                    builder.switch_to_block(block_check_end);
                    let end_idx = builder.ins().iadd_imm(evm_offset, 32);
                    let is_partial =
                        builder
                            .ins()
                            .icmp(IntCC::UnsignedGreaterThan, end_idx, input_len);
                    builder
                        .ins()
                        .brif(is_partial, block_partial, &[], block_fast, &[]);

                    // --- [2] Block Zero: 0 채우기 (I64 x 4) ---
                    // I128 Store는 스택 정렬이 안 맞으면 터질 수 있으므로 I64로 안전하게 처리
                    builder.switch_to_block(block_zero);
                    // let zero = builder.ins().iconst(types::I64, 0);

                    builder.ins().store(mem, zero, ptr_top, 0);
                    builder.ins().store(mem, zero, ptr_top, 8);
                    builder.ins().store(mem, zero, ptr_top, 16);
                    builder.ins().store(mem, zero, ptr_top, 24);

                    builder.ins().jump(block_done, &[]);

                    // --- [3] Fast Path: 32바이트 복사 (I64 x 4) ---
                    // Input Pointer는 16바이트 정렬이 보장되지 않으므로 I128 Load는 위험함
                    builder.switch_to_block(block_fast);
                    let src_ptr = builder.ins().iadd(input_ptr, evm_offset);

                    // High (Offset 24)
                    let val_hi_be = builder.ins().load(types::I64, mem, src_ptr, 0);
                    let val_hi_le = builder.ins().bswap(val_hi_be);
                    builder.ins().store(mem, val_hi_le, ptr_top, 24);

                    // Mid1 (Offset 16)
                    let val_mid1_be = builder.ins().load(types::I64, mem, src_ptr, 8);
                    let val_mid1_le = builder.ins().bswap(val_mid1_be);
                    builder.ins().store(mem, val_mid1_le, ptr_top, 16);

                    // Mid2 (Offset 8)
                    let val_mid2_be = builder.ins().load(types::I64, mem, src_ptr, 16);
                    let val_mid2_le = builder.ins().bswap(val_mid2_be);
                    builder.ins().store(mem, val_mid2_le, ptr_top, 8);

                    // Low (Offset 0)
                    let val_lo_be = builder.ins().load(types::I64, mem, src_ptr, 24);
                    let val_lo_le = builder.ins().bswap(val_lo_be);
                    builder.ins().store(mem, val_lo_le, ptr_top, 0);

                    builder.ins().jump(block_done, &[]);

                    // --- [4] Partial Path: 바이트 루프 (Inline) ---
                    builder.switch_to_block(block_partial);
                    // 1. 선제적 0 초기화 (I64 x 4)
                    builder.ins().store(mem, zero, ptr_top, 0);
                    builder.ins().store(mem, zero, ptr_top, 8);
                    builder.ins().store(mem, zero, ptr_top, 16);
                    builder.ins().store(mem, zero, ptr_top, 24);

                    // 2. 루프 로직 (기존과 동일)
                    let remaining = builder.ins().isub(input_len, evm_offset);
                    let loop_header = builder.create_block();
                    let loop_body = builder.create_block();
                    let loop_exit = builder.create_block();

                    let idx_init = builder.ins().iconst(types::I64, 0);
                    builder.ins().jump(loop_header, &[idx_init]);

                    // Loop Header
                    builder.switch_to_block(loop_header);
                    builder.append_block_param(loop_header, types::I64); // 이 줄 추가 필요
                    let idx = builder.block_params(loop_header)[0];
                    let loop_cond =
                        builder
                            .ins()
                            .icmp(IntCC::UnsignedGreaterThanOrEqual, idx, remaining);
                    builder
                        .ins()
                        .brif(loop_cond, loop_exit, &[], loop_body, &[]);

                    // Loop Body
                    builder.switch_to_block(loop_body);
                    let src_offset = builder.ins().iadd(evm_offset, idx);
                    let byte_ptr = builder.ins().iadd(input_ptr, src_offset);
                    let byte_val = builder.ins().load(types::I8, mem, byte_ptr, 0);

                    let const_31 = builder.ins().iconst(types::I64, 31);
                    let dst_idx = builder.ins().isub(const_31, idx);
                    let dst_ptr = builder.ins().iadd(ptr_top, dst_idx);

                    builder.ins().store(mem, byte_val, dst_ptr, 0);

                    let idx_next = builder.ins().iadd_imm(idx, 1);
                    builder.ins().jump(loop_header, &[idx_next]);

                    // Loop Exit
                    builder.switch_to_block(loop_exit);
                    builder.ins().jump(block_done, &[]);

                    // --- Finish ---
                    builder.switch_to_block(block_done);

                    builder.seal_block(block_check_range);
                    builder.seal_block(block_check_end);
                    builder.seal_block(block_zero);
                    builder.seal_block(block_fast);
                    builder.seal_block(block_partial);
                    builder.seal_block(loop_header);
                    builder.seal_block(loop_body);
                    builder.seal_block(loop_exit);
                    builder.seal_block(block_done);
                }

                // CALLDATASIZE (0x36)
                0x36 => {
                    // Stack: Push 1 (input_len)
                    let ptr = builder.ins().iadd_imm(stack_cursor, offset as i64);
                    let mem = MemFlags::new();
                    let zero = builder.ins().iconst(types::I64, 0);

                    // [Native Little Endian Layout]
                    // transmute 방식을 쓰기로 했으므로, x86/ARM에서는
                    // 가장 낮은 자릿수(Low 64bit)가 오프셋 0번지에 와야 합니다.

                    builder.ins().store(mem, input_len, ptr, 0); // w0 (실제 값)
                    builder.ins().store(mem, zero, ptr, 8); // w1 (0)
                    builder.ins().store(mem, zero, ptr, 16); // w2 (0)
                    builder.ins().store(mem, zero, ptr, 24); // w3 (0)

                    offset += 32;
                }

                // -------------------------------------------------------------
                // [Stack Operations]
                // -------------------------------------------------------------

                // POP
                0x50 => {
                    offset -= 32;
                }

                // MLOAD
                0x51 => {
                    // Stack: [..., offset] -> [..., value]

                    let mem = MemFlags::new();

                    // 1. 오프셋 가져오기
                    let offset_ptr = builder.ins().iadd_imm(stack_cursor, (offset - 32) as i64);
                    let mem_offset = builder.ins().load(types::I64, mem, offset_ptr, 0);

                    // 2. 메모리 주소 계산
                    let src_ptr = builder.ins().iadd(memory_ptr, mem_offset);

                    // 3. [최적화] 128비트 단위로 로드 (2번만 수행)
                    // EVM Memory (Big Endian): [High 128bit (MSB)] [Low 128bit (LSB)]
                    // Stack (Little Endian):   [Low 128bit (LSB)]  [High 128bit (MSB)]

                    // (1) High Part (MSB): Memory + 0  -> Stack + 16
                    // load.i128은 x86에서 XMM 레지스터를 사용 (SSE/AVX)
                    let val_msb_be = builder.ins().load(types::I128, mem, src_ptr, 0);
                    let val_msb = builder.ins().bswap(val_msb_be); // 128비트 전체 byte swap (PSHUFB 등으로 변환됨)
                    builder.ins().store(mem, val_msb, offset_ptr, 16);

                    // (2) Low Part (LSB): Memory + 16 -> Stack + 0
                    let val_lsb_be = builder.ins().load(types::I128, mem, src_ptr, 16);
                    let val_lsb = builder.ins().bswap(val_lsb_be);
                    builder.ins().store(mem, val_lsb, offset_ptr, 0);
                }

                // MSTORE
                0x52 => {
                    // Stack: [..., value, offset]

                    let mem = MemFlags::new();

                    let off_ptr_stack = builder.ins().iadd_imm(stack_cursor, (offset - 32) as i64);
                    let val_ptr_stack = builder.ins().iadd_imm(stack_cursor, (offset - 64) as i64);

                    let mem_offset = builder.ins().load(types::I64, mem, off_ptr_stack, 0);
                    let dst_ptr = builder.ins().iadd(memory_ptr, mem_offset);

                    // [최적화] Stack(Little) -> Bswap -> Memory(Big)

                    // (1) High Part: Stack + 16 -> Memory + 0
                    let val_msb = builder.ins().load(types::I128, mem, val_ptr_stack, 16);
                    let val_msb_be = builder.ins().bswap(val_msb);
                    builder.ins().store(mem, val_msb_be, dst_ptr, 0);

                    // (2) Low Part: Stack + 0  -> Memory + 16
                    let val_lsb = builder.ins().load(types::I128, mem, val_ptr_stack, 0);
                    let val_lsb_be = builder.ins().bswap(val_lsb);
                    builder.ins().store(mem, val_lsb_be, dst_ptr, 16);

                    offset -= 64; // Pop 2
                }

                // MSTORE8
                0x53 => {
                    let mem = MemFlags::new();
                    let off_ptr_stack = builder.ins().iadd_imm(stack_cursor, (offset - 32) as i64);
                    let val_ptr_stack = builder.ins().iadd_imm(stack_cursor, (offset - 64) as i64);

                    let mem_offset = builder.ins().load(types::I64, mem, off_ptr_stack, 0);
                    let dst_ptr = builder.ins().iadd(memory_ptr, mem_offset);
                    let val_byte = builder.ins().load(types::I8, mem, val_ptr_stack, 0);
                    builder.ins().store(mem, val_byte, dst_ptr, 0);

                    offset -= 64; // Pop 2
                }

                // MSIZE
                0x59 => {
                    // Stack: [] -> [size] (Push 1)
                    let ptr = builder.ins().iadd_imm(stack_cursor, offset as i64);
                    let mem = MemFlags::new();
                    let zero = builder.ins().iconst(types::I64, 0);

                    // 1. memory_len (u64) 저장 (Little Endian LSB)
                    // EVM은 32바이트 word 단위가 아니라 바이트 단위 크기를 반환합니다.
                    // (Go Runtime에서 evm.Memory.Len() 값을 넘겨줬다고 가정)
                    builder.ins().store(mem, memory_len, ptr, 0);

                    // 2. 나머지 상위 24바이트 0으로 채우기
                    builder.ins().store(mem, zero, ptr, 8);
                    builder.ins().store(mem, zero, ptr, 16);
                    builder.ins().store(mem, zero, ptr, 24);

                    offset += 32;
                }

                // PUSH0
                0x5f => {
                    let ptr = builder.ins().iadd_imm(stack_cursor, offset as i64);
                    let zero = builder.ins().iconst(types::I128, 0); // Use i128 for speed
                    let mem = MemFlags::new();
                    builder.ins().store(mem, zero, ptr, 0);
                    builder.ins().store(mem, zero, ptr, 16);
                    offset += 32;
                }

                // PUSH1..32
                op if (0x60..=0x7f).contains(&op) => {
                    let size = (op - 0x60 + 1) as usize;
                    let mut buf = [0u8; 32];
                    if cursor + size <= bytecode.len() {
                        let raw = &bytecode[cursor..cursor + size];
                        // EVM Big Endian -> Buffer End Alignment
                        let start = 32 - size;
                        buf[start..].copy_from_slice(raw);
                    }
                    cursor += size;

                    // let u = ethnum::U256::from_be_bytes(buf);
                    // let (low, high) = u.into_words();
                    // let w0 = low as u64;
                    // let w1 = (low >> 64) as u64;
                    // let w2 = high as u64;
                    // let w3 = (high >> 64) as u64;

                    // let ptr = builder.ins().iadd_imm(stack_cursor, offset as i64);
                    // let mem = MemFlags::new();

                    // // Unrolled store (Little Endian)
                    // let v0 = builder.ins().iconst(types::I64, w2 as i64);
                    // builder.ins().store(mem, v0, ptr, 0);

                    // let v1 = builder.ins().iconst(types::I64, w3 as i64);
                    // builder.ins().store(mem, v1, ptr, 8);

                    // let v2 = builder.ins().iconst(types::I64, w0 as i64);
                    // builder.ins().store(mem, v2, ptr, 16);

                    // let v3 = builder.ins().iconst(types::I64, w1 as i64);
                    // builder.ins().store(mem, v3, ptr, 24);

                    // 2. Bytes -> U256 (Integer 값 복원)
                    let val = ethnum::U256::from_be_bytes(buf);

                    // 3. [핵심 변경] U256 -> [u64; 4] (Native Layout 변환)
                    // into_words()나 비트 시프트를 직접 할 필요 없습니다.
                    // 현재 머신(x86/ARM)이 Little Endian이라면,
                    // 자동으로 words[0]에 가장 낮은 자릿수(LSB)가 들어갑니다.
                    let words: [u64; 4] = unsafe { std::mem::transmute(val) };

                    // 4. Cranelift IR 생성 (순서대로 저장)
                    let ptr = builder.ins().iadd_imm(stack_cursor, offset as i64);
                    let mem = MemFlags::new();

                    // words[0] -> Offset +0  (Little Endian의 LSB)
                    // words[1] -> Offset +8
                    // words[2] -> Offset +16
                    // words[3] -> Offset +24 (Little Endian의 MSB)
                    for (i, &word) in words.iter().enumerate() {
                        let v = builder.ins().iconst(types::I64, word as i64);
                        builder.ins().store(mem, v, ptr, (i * 8) as i32);
                    }

                    offset += 32;
                }

                // DUP (i128 x 2)
                op if (0x80..=0x8f).contains(&op) => {
                    let depth = (op - 0x80 + 1) as i32;
                    let src_off = offset - (depth * 32);

                    let src_ptr = builder.ins().iadd_imm(stack_cursor, src_off as i64);
                    let dst_ptr = builder.ins().iadd_imm(stack_cursor, offset as i64);
                    let mem = MemFlags::new();

                    let v0 = builder.ins().load(types::I128, mem, src_ptr, 0);
                    let v1 = builder.ins().load(types::I128, mem, src_ptr, 16);
                    builder.ins().store(mem, v0, dst_ptr, 0);
                    builder.ins().store(mem, v1, dst_ptr, 16);
                    offset += 32;
                }

                // SWAP (i128 x 2)
                op if (0x90..=0x9f).contains(&op) => {
                    let depth = (op - 0x90 + 1) as i32;
                    let off1 = offset - 32;
                    let off2 = offset - ((depth + 1) * 32);

                    let ptr1 = builder.ins().iadd_imm(stack_cursor, off1 as i64);
                    let ptr2 = builder.ins().iadd_imm(stack_cursor, off2 as i64);
                    let mem = MemFlags::new();

                    let v1_lo = builder.ins().load(types::I128, mem, ptr1, 0);
                    let v2_lo = builder.ins().load(types::I128, mem, ptr2, 0);
                    builder.ins().store(mem, v2_lo, ptr1, 0);
                    builder.ins().store(mem, v1_lo, ptr2, 0);

                    let v1_hi = builder.ins().load(types::I128, mem, ptr1, 16);
                    let v2_hi = builder.ins().load(types::I128, mem, ptr2, 16);
                    builder.ins().store(mem, v2_hi, ptr1, 16);
                    builder.ins().store(mem, v1_hi, ptr2, 16);
                }

                // PC (0x58)
                0x58 => {
                    let pc_val = current_real_pc;
                    let ptr = builder.ins().iadd_imm(stack_cursor, offset as i64);
                    let v_pc = builder.ins().iconst(types::I64, pc_val as i64);
                    let zero = builder.ins().iconst(types::I64, 0);
                    let mem = MemFlags::new();

                    builder.ins().store(mem, v_pc, ptr, 0);
                    builder.ins().store(mem, zero, ptr, 8);
                    builder.ins().store(mem, zero, ptr, 16);
                    builder.ins().store(mem, zero, ptr, 24);
                    offset += 32;
                }

                // Flow (JUMP, JUMPI - Handled by Go)
                0x56 => {
                    // NOTE: if analyzer determines that dynamic jump is allowed, then this `pop`
                    // behavior is required to make a stack be consistent
                    offset -= 32;
                }
                // 0x5b => {}
                _ => {
                    unimplemented!("{}", format!("unimplemented opcode: {:x}", op));
                }
            }
        }
        builder.ins().return_(&[]);
        builder.finalize();
        let res = verify_function(&ctx.func, &*engine.isa);
        if let Err(errors) = res {
            if !err_out.is_null() {
                let error_msg = format!("IR Verifier Error: {}", errors);
                let c_str = CString::new(error_msg).unwrap();
                unsafe {
                    *err_out = c_str.into_raw();
                }
            }
            return ptr::null_mut();
        }
    }

    engine.counter += 1;

    // {
    //     let func_name = format!("trace_{}", engine.counter);
    //     dump_cranelift_ir(&ctx.func, &func_name);
    // }

    let id = engine
        .module
        .declare_function(
            &format!("trace_{}", engine.counter),
            Linkage::Export,
            &ctx.func.signature,
        )
        .unwrap();
    engine.module.define_function(id, ctx).unwrap();
    engine.module.clear_context(ctx);
    engine.module.finalize_definitions().unwrap();

    // {
    // let code_len = ctx.compiled_code().unwrap().buffer.data().len();
    //     let code_ptr = engine.module.get_finalized_function(id);
    //     dump::dump_machine_code(code_ptr, code_len);
    // }
    engine.module.get_finalized_function(id)
}

fn get_runtime_func(
    module: &mut JITModule,
    funcs: &mut HashMap<String, FuncId>,
    name: &str,
    num_args: usize,
) -> FuncId {
    if let Some(id) = funcs.get(name) {
        return *id;
    }
    let mut sig = module.make_signature();
    sig.call_conv = module.target_config().default_call_conv;
    for _ in 0..num_args {
        sig.params.push(AbiParam::new(types::I64));
    }
    let id = module
        .declare_function(name, Linkage::Import, &sig)
        .unwrap();
    funcs.insert(name.to_string(), id);
    id
}
