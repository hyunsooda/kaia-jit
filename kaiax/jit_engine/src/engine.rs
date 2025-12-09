use crate::dump;
use crate::runtime;
use cranelift::prelude::*;
use cranelift_jit::{JITBuilder, JITModule};
use cranelift_module::{FuncId, Linkage, Module};
use std::collections::HashMap;
use std::slice;

// -----------------------------------------------------------------------------
// Init & Structs
// -----------------------------------------------------------------------------

pub struct JitEngine {
    builder_context: FunctionBuilderContext,
    ctx: codegen::Context,
    module: JITModule,
    counter: u32,
    // Runtime 함수 ID 캐시 (매번 import 방지)
    funcs: HashMap<String, FuncId>,
}

#[no_mangle]
pub extern "C" fn new_jit_engine() -> *mut JitEngine {
    let mut flag_builder = settings::builder();
    flag_builder.set("use_colocated_libcalls", "false").unwrap();
    flag_builder.set("is_pic", "true").unwrap();

    let isa_builder = cranelift_native::builder().unwrap_or_else(|msg| {
        panic!("host machine is not supported: {}", msg);
    });
    let isa = isa_builder
        .finish(settings::Flags::new(flag_builder))
        .unwrap();

    let mut builder = JITBuilder::with_isa(isa, cranelift_module::default_libcall_names());

    // [핵심] Host Function 주소 등록 (Symbol Lookup)
    // JIT 코드가 "jit_add"를 호출하면 -> 실제 runtime.rs의 jit_add 주소로 연결

    // 심볼 등록 매크로 (반복 줄이기)
    macro_rules! register {
        ($name:expr, $func:ident) => {
            builder.symbol($name, runtime::$func as *const u8);
        };
    }

    // runtime.rs의 모든 함수 등록
    // register!("jit_add", jit_add);
    // register!("jit_sub", jit_sub);
    register!("jit_mul", jit_mul);
    register!("jit_div", jit_div);
    register!("jit_sdiv", jit_sdiv);
    register!("jit_mod", jit_mod);
    register!("jit_smod", jit_smod);
    register!("jit_addmod", jit_addmod);
    register!("jit_mulmod", jit_mulmod);
    register!("jit_signextend", jit_signextend);
    // register!("jit_lt", jit_lt);
    // register!("jit_gt", jit_gt);
    // register!("jit_slt", jit_slt);
    // register!("jit_sgt", jit_sgt);
    // register!("jit_eq", jit_eq);
    // register!("jit_iszero", jit_iszero);
    // register!("jit_and", jit_and);
    // register!("jit_or", jit_or);
    // register!("jit_xor", jit_xor);
    // register!("jit_not", jit_not);
    register!("jit_byte", jit_byte);
    register!("jit_shl", jit_shl);
    register!("jit_shr", jit_shr);
    register!("jit_sar", jit_sar);
    register!("jit_calldataload", jit_calldataload);

    let module = JITModule::new(builder);

    let engine = JitEngine {
        builder_context: FunctionBuilderContext::new(),
        ctx: module.make_context(),
        module,
        counter: 0,
        funcs: HashMap::new(),
    };

    Box::into_raw(Box::new(engine))
}

#[no_mangle]
pub extern "C" fn free_jit_engine(ptr: *mut JitEngine) {
    if !ptr.is_null() {
        unsafe {
            let _ = Box::from_raw(ptr);
        }
    }
}

// -----------------------------------------------------------------------------
// Helper Methods
// -----------------------------------------------------------------------------

// 인자가 2개인 함수 가져오기
fn get_func_2(module: &mut JITModule, funcs: &mut HashMap<String, FuncId>, name: &str) -> FuncId {
    if let Some(id) = funcs.get(name) {
        return *id;
    }
    let mut sig = module.make_signature();
    sig.call_conv = module.target_config().default_call_conv;
    sig.params.push(AbiParam::new(types::I64));
    sig.params.push(AbiParam::new(types::I64));

    let id = module
        .declare_function(name, Linkage::Import, &sig)
        .unwrap();
    funcs.insert(name.to_string(), id);
    id
}

// 인자가 3개인 함수 가져오기
fn get_func_3(module: &mut JITModule, funcs: &mut HashMap<String, FuncId>, name: &str) -> FuncId {
    if let Some(id) = funcs.get(name) {
        return *id;
    }
    let mut sig = module.make_signature();
    sig.call_conv = module.target_config().default_call_conv;
    sig.params.push(AbiParam::new(types::I64));
    sig.params.push(AbiParam::new(types::I64));
    sig.params.push(AbiParam::new(types::I64));

    let id = module
        .declare_function(name, Linkage::Import, &sig)
        .unwrap();
    funcs.insert(name.to_string(), id);
    id
}

// 인자가 1개인 함수 가져오기
fn get_func_1(module: &mut JITModule, funcs: &mut HashMap<String, FuncId>, name: &str) -> FuncId {
    if let Some(id) = funcs.get(name) {
        return *id;
    }
    let mut sig = module.make_signature();
    sig.call_conv = module.target_config().default_call_conv;
    sig.params.push(AbiParam::new(types::I64));

    let id = module
        .declare_function(name, Linkage::Import, &sig)
        .unwrap();
    funcs.insert(name.to_string(), id);
    id
}

// impl JitEngine {
//     // 런타임 함수 Import Helper
//     fn get_func(&mut self, name: &str, num_args: usize) -> FuncId {
//         if let Some(id) = self.funcs.get(name) {
//             return *id;
//         }

//         let mut sig = self.module.make_signature();
//         sig.call_conv = self.module.target_config().default_call_conv;

//         // 인자는 모두 포인터(i64)로 통일
//         for _ in 0..num_args {
//             sig.params.push(AbiParam::new(types::I64));
//         }
//         // 리턴값 없음 (void)

//         let id = self
//             .module
//             .declare_function(name, Linkage::Import, &sig)
//             .unwrap();
//         self.funcs.insert(name.to_string(), id);
//         id
//     }

//     // 인자가 1개인 함수 (ISZERO, NOT)
//     fn get_unary_func(&mut self, name: &str) -> FuncId {
//         if let Some(id) = self.funcs.get(name) {
//             return *id;
//         }

//         let mut sig = self.module.make_signature();
//         sig.call_conv = self.module.target_config().default_call_conv;
//         sig.params.push(AbiParam::new(types::I64));

//         let id = self
//             .module
//             .declare_function(name, Linkage::Import, &sig)
//             .unwrap();
//         self.funcs.insert(name.to_string(), id);
//         id
//     }

//     // 인자가 3개인 함수 (ADDMOD, MULMOD)
//     fn get_ternary_func(&mut self, name: &str) -> FuncId {
//         if let Some(id) = self.funcs.get(name) {
//             return *id;
//         }

//         let mut sig = self.module.make_signature();
//         sig.call_conv = self.module.target_config().default_call_conv;
//         sig.params.push(AbiParam::new(types::I64));
//         sig.params.push(AbiParam::new(types::I64));
//         sig.params.push(AbiParam::new(types::I64));

//         let id = self
//             .module
//             .declare_function(name, Linkage::Import, &sig)
//             .unwrap();
//         self.funcs.insert(name.toadd_string(), id);
//         id
//     }
// }

// -----------------------------------------------------------------------------
// Compiler
// -----------------------------------------------------------------------------

#[no_mangle]
pub extern "C" fn compile_trace(
    ptr: *mut JitEngine,
    code_ptr: *const u8,
    pc_map_ptr: *const u64,
    len: usize,
) -> *const u8 {
    let engine = unsafe { &mut *ptr };
    let ctx = &mut engine.ctx;
    let module = &mut engine.module;
    let funcs = &mut engine.funcs;

    // Go가 스티칭해서 넘겨준 '선형 바이트코드'
    let bytecode = unsafe { slice::from_raw_parts(code_ptr, len) };
    let pc_map = unsafe { slice::from_raw_parts(pc_map_ptr, len) };

    module.clear_context(ctx);

    // Function Signature: fn(stack_cursor: *mut u64)
    let ptr_type = module.target_config().pointer_type();
    ctx.func.signature.params.push(AbiParam::new(ptr_type)); // stack_cursor (param 0)
    ctx.func.signature.params.push(AbiParam::new(ptr_type)); // input ptr (param 1) [추가]
    ctx.func.signature.params.push(AbiParam::new(types::I64)); // input length (param 2) [추가]

    // println!("START!!");
    {
        let mut builder = FunctionBuilder::new(&mut ctx.func, &mut engine.builder_context);
        let entry = builder.create_block();
        builder.append_block_params_for_function_params(entry);
        builder.switch_to_block(entry);

        let params = builder.block_params(entry);
        let stack_cursor = params[0];
        let input_ptr = params[1]; // [추가]
        let input_len = params[2]; // [추가]

        // TODO: remove me
        // // Go에서 넘겨준 Stack Pointer (현재 유효 데이터의 바로 위 = 0점)
        // let stack_cursor = builder.block_params(entry)[0];

        // JIT 내부의 가상 오프셋 (Virtual Offset)
        // 0: Cursor 위치 (Top+1)
        // -32: Top
        // -64: Top-1
        let mut offset: i32 = 0;

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

        // 바이트코드 순회 (PC가 아니라 Cursor)
        let mut cursor = 0;

        let mut cnt = 0;
        while cursor < bytecode.len() {
            let op = bytecode[cursor];
            let current_real_pc = pc_map[cursor];
            cursor += 1;
            cnt += 1;

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

                    //                     let ptr_dest = builder.ins().iadd_imm(stack_cursor, (offset - 64) as i64);
                    //                     let ptr_src = builder.ins().iadd_imm(stack_cursor, (offset - 32) as i64);

                    //                     let fid = get_func_2(module, funcs, "jit_add");
                    //                     let func_ref = module.declare_func_in_func(fid, builder.func);
                    //                     builder.ins().call(func_ref, &[ptr_dest, ptr_src]);

                    //                     offset -= 32; // 스택 1칸 줄어듦
                }
                0x03 => {
                    // SUB (EVM: a - b, stack: [a, b(top)])
                    let top = load_i64_x4(&mut builder, offset - 32);
                    let second = load_i64_x4(&mut builder, offset - 64);
                    let res = InlineOps::u256_sub(&mut builder, top, second);
                    store_i64_x4(&mut builder, offset - 64, res);
                    offset -= 32;

                    // let ptr_dest = builder.ins().iadd_imm(stack_cursor, (offset - 64) as i64);
                    // let ptr_src = builder.ins().iadd_imm(stack_cursor, (offset - 32) as i64);

                    // let fid = get_func_2(module, funcs, "jit_sub");
                    // let func_ref = module.declare_func_in_func(fid, builder.func);
                    // builder.ins().call(func_ref, &[ptr_dest, ptr_src]);

                    // offset -= 32; // 스택 1칸 줄어듦
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
                // [Inline] Comparison (LT, GT, EQ) -> 순서 매우 중요!
                // -------------------------------------------------------------
                0x10 => {
                    // LT (Top < Second)
                    let second = load_i64_x4(&mut builder, offset - 64);
                    let top = load_i64_x4(&mut builder, offset - 32);

                    // [수정] Top < Second
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
                        0x1b => "jit_shl",
                        0x1c => "jit_shr",
                        0x1d => "jit_sar",
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
                0x35 => {
                    let fid = get_runtime_func(module, funcs, "jit_calldataload", 3);
                    let func_ref = module.declare_func_in_func(fid, builder.func);
                    let ptr_top = builder.ins().iadd_imm(stack_cursor, (offset - 32) as i64);
                    builder
                        .ins()
                        .call(func_ref, &[ptr_top, input_ptr, input_len]);
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
                    offset -= 32;
                }
                0x57 => {
                    offset -= 64;
                }
                0x5b => {}

                _ => {} // // -------------------------------------------------------------
                        // // [Group 1] Binary Ops (Pop 2, Push 1) -> Net -32 bytes
                        // // -------------------------------------------------------------
                        // // ADD, MUL, SUB, DIV, SDIV, MOD, SMOD, EXP, SIGNEXTEND
                        // // LT, GT, SLT, SGT, EQ, AND, OR, XOR, BYTE, SHL, SHR, SAR
                        // 0x01..=0x07 | 0x0a | 0x0b | 0x10..=0x14 | 0x16..=0x18 | 0x1a..=0x1d => {
                        //     let func_name = match op {
                        //         0x01 => "jit_add",
                        //         0x02 => "jit_mul",
                        //         0x03 => "jit_sub",
                        //         0x04 => "jit_div",
                        //         0x05 => "jit_sdiv",
                        //         0x06 => "jit_mod",
                        //         0x07 => "jit_smod",
                        //         0x0b => "jit_signextend",
                        //         0x10 => "jit_lt",
                        //         0x11 => "jit_gt",
                        //         0x12 => "jit_slt",
                        //         0x13 => "jit_sgt",
                        //         0x14 => "jit_eq",
                        //         0x16 => "jit_and",
                        //         0x17 => "jit_or",
                        //         0x18 => "jit_xor",
                        //         0x1a => "jit_byte",
                        //         0x1b => "jit_shl",
                        //         0x1c => "jit_shr",
                        //         0x1d => "jit_sar",
                        //         _ => unreachable!(),
                        //     };

                        //     // Param 1: Top-1 (결과가 저장될 곳) -> offset - 64
                        //     // Param 2: Top   (읽을 값)         -> offset - 32
                        //     let ptr_dest = builder.ins().iadd_imm(stack_cursor, (offset - 64) as i64);
                        //     let ptr_src = builder.ins().iadd_imm(stack_cursor, (offset - 32) as i64);

                        //     let fid = get_func_2(module, funcs, func_name);
                        //     let func_ref = module.declare_func_in_func(fid, builder.func);
                        //     builder.ins().call(func_ref, &[ptr_dest, ptr_src]);

                        //     offset -= 32; // 스택 1칸 줄어듦

                        //     // println!("[{:#x}]({}): {}", current_real_pc, offset, func_name);
                        // }

                        // // -------------------------------------------------------------
                        // // [Group 2] Ternary Ops (Pop 3, Push 1) -> Net -64 bytes
                        // // -------------------------------------------------------------
                        // // ADDMOD, MULMOD
                        // 0x08 | 0x09 => {
                        //     let func_name = if op == 0x08 {
                        //         "jit_addmod"
                        //     } else {
                        //         "jit_mulmod"
                        //     };

                        //     // Param 1: Top-2 (Dest) -> offset - 96
                        //     // Param 2: Top-1 (Mid)  -> offset - 64
                        //     // Param 3: Top   (Top)  -> offset - 32
                        //     let ptr_dest = builder.ins().iadd_imm(stack_cursor, (offset - 96) as i64);
                        //     let ptr_mid = builder.ins().iadd_imm(stack_cursor, (offset - 64) as i64);
                        //     let ptr_top = builder.ins().iadd_imm(stack_cursor, (offset - 32) as i64);

                        //     let fid = get_func_3(module, funcs, func_name);
                        //     let func_ref = module.declare_func_in_func(fid, builder.func);
                        //     builder.ins().call(func_ref, &[ptr_dest, ptr_mid, ptr_top]);

                        //     offset -= 64; // 스택 2칸 줄어듦

                        //     // println!("[{:#x}]({}): {}", current_real_pc, offset, func_name);
                        // }

                        // // -------------------------------------------------------------
                        // // [Group 3] Unary Ops (Pop 1, Push 1) -> Net 0 bytes
                        // // -------------------------------------------------------------
                        // // ISZERO, NOT
                        // 0x15 | 0x19 => {
                        //     let func_name = if op == 0x15 { "jit_iszero" } else { "jit_not" };

                        //     // Param 1: Top (Dest & Src) -> offset - 32
                        //     let ptr_top = builder.ins().iadd_imm(stack_cursor, (offset - 32) as i64);

                        //     let fid = get_func_1(module, funcs, func_name);
                        //     let func_ref = module.declare_func_in_func(fid, builder.func);
                        //     builder.ins().call(func_ref, &[ptr_top]);

                        //     // println!("[{:#x}]({}): {}", current_real_pc, offset, func_name);
                        //     // offset 변화 없음
                        // }

                        // // -------------------------------------------------------------
                        // // [Group 4] Stack Operations (Inline)
                        // // -------------------------------------------------------------

                        // // POP (Pop 1)
                        // 0x50 => {
                        //     // println!("[{:#x}]({}): jit_pop1", current_real_pc, offset);
                        //     offset -= 32;
                        // }

                        // // PUSH0 (Push 1)
                        // 0x5f => {
                        //     // let ptr = builder.ins().iadd_imm(stack_cursor, offset as i64);
                        //     // let zero = builder.ins().iconst(types::I64, 0);
                        //     // let mem = MemFlags::new();
                        //     // // 32바이트(4x8) 0으로 초기화
                        //     // for i in 0..4 {
                        //     //     builder.ins().store(mem, zero, ptr, i * 8);
                        //     // }
                        //     let ptr = builder.ins().iadd_imm(stack_cursor, offset as i64);
                        //     let zero = builder.ins().iconst(types::I128, 0); // 128-bit Zero
                        //     let mem = MemFlags::new();

                        //     // 0~15 바이트 (Low 128bit)
                        //     builder.ins().store(mem, zero, ptr, 0);
                        //     // 16~31 바이트 (High 128bit)
                        //     builder.ins().store(mem, zero, ptr, 16);
                        //     offset += 32;
                        //     // println!("[{:#x}]({}): jit_push0", current_real_pc, offset);
                        // }

                        // // PUSH1..32
                        // op if (0x60..=0x7f).contains(&op) => {
                        //     let size = (op - 0x60 + 1) as usize;

                        //     // 32바이트 버퍼에 데이터 로드 (Big Endian -> Host Endian 변환)
                        //     let mut buf = [0u8; 32];
                        //     if cursor + size <= bytecode.len() {
                        //         let raw = &bytecode[cursor..cursor + size];
                        //         // 오른쪽 정렬 (EVM Big Endian)
                        //         // 예: PUSH1 0x01 -> [0...01]
                        //         let start = 32 - size;
                        //         buf[start..].copy_from_slice(raw);
                        //     }
                        //     cursor += size; // 데이터 바이트 건너뜀

                        //     // ethnum U256을 이용해 Host Endian(Little) u64 배열로 변환
                        //     let u = ethnum::U256::from_be_bytes(buf);
                        //     let words = u.into_words(); // (low u128, high u128)
                        //     let w = [
                        //         // words.0 as u64,         // word 0
                        //         // (words.0 >> 64) as u64, // word 1
                        //         // words.1 as u64,         // word 2
                        //         // (words.1 >> 64) as u64, // word 3
                        //         words.1 as u64,         // word 2
                        //         (words.1 >> 64) as u64, // word 3
                        //         words.0 as u64,         // word 0
                        //         (words.0 >> 64) as u64, // word 1
                        //     ];

                        //     let ptr = builder.ins().iadd_imm(stack_cursor, offset as i64);
                        //     let mem = MemFlags::new();

                        //     // 메모리에 쓰기
                        //     for (i, &word) in w.iter().enumerate() {
                        //         let val = builder.ins().iconst(types::I64, word as i64);
                        //         builder.ins().store(mem, val, ptr, (i as i32) * 8);
                        //     }
                        //     offset += 32;
                        //     // println!("[{:#x}]({}): jit_pushN", current_real_pc, offset);
                        // }

                        // // DUP1..16
                        // op if (0x80..=0x8f).contains(&op) => {
                        //     // let depth = (op - 0x80 + 1) as i32;
                        //     // // Src: Top - depth (Current - 32*depth) -> (offset - 32) - 32*(depth-1) ??
                        //     // // DUP1: Top 복사. offset-32.
                        //     // // Cursor 기준: DUP1 읽을 위치는 (offset - 32).
                        //     // // DUP2 읽을 위치는 (offset - 64).
                        //     // let src_off = offset - (depth * 32);

                        //     // let src_ptr = builder.ins().iadd_imm(stack_cursor, src_off as i64);
                        //     // let dst_ptr = builder.ins().iadd_imm(stack_cursor, offset as i64);

                        //     // let mem = MemFlags::new();
                        //     // for i in 0..4 {
                        //     //     let v = builder.ins().load(types::I64, mem, src_ptr, i * 8);
                        //     //     builder.ins().store(mem, v, dst_ptr, i * 8);
                        //     // }
                        //     let depth = (op - 0x80 + 1) as i32;
                        //     let src_off = offset - (depth * 32);

                        //     let src_ptr = builder.ins().iadd_imm(stack_cursor, src_off as i64);
                        //     let dst_ptr = builder.ins().iadd_imm(stack_cursor, offset as i64);
                        //     let mem = MemFlags::new();

                        //     // Part 1: Low 128 bit
                        //     let val_lo = builder.ins().load(types::I128, mem, src_ptr, 0);
                        //     builder.ins().store(mem, val_lo, dst_ptr, 0);

                        //     // Part 2: High 128 bit
                        //     let val_hi = builder.ins().load(types::I128, mem, src_ptr, 16);
                        //     builder.ins().store(mem, val_hi, dst_ptr, 16);
                        //     offset += 32;
                        //     // println!("[{:#x}]({}): jit_dupN", current_real_pc, offset);
                        // }

                        // // SWAP1..16
                        // op if (0x90..=0x9f).contains(&op) => {
                        //     // let depth = (op - 0x90 + 1) as i32;
                        //     // // Top: offset - 32
                        //     // // Target: offset - (depth + 1) * 32
                        //     // let off1 = offset - 32;
                        //     // let off2 = offset - ((depth + 1) * 32);

                        //     // let ptr1 = builder.ins().iadd_imm(stack_cursor, off1 as i64);
                        //     // let ptr2 = builder.ins().iadd_imm(stack_cursor, off2 as i64);

                        //     // let mem = MemFlags::new();
                        //     // for i in 0..4 {
                        //     //     let v1 = builder.ins().load(types::I64, mem, ptr1, i * 8);
                        //     //     let v2 = builder.ins().load(types::I64, mem, ptr2, i * 8);
                        //     //     let v1 = builder.ins().load(types::I64, mem, ptr1, i * 8);
                        //     //     let v2 = builder.ins().load(types::I64, mem, ptr2, i * 8);
                        //     //     builder.ins().store(mem, v2, ptr1, i * 8);
                        //     //     builder.ins().store(mem, v1, ptr2, i * 8);
                        //     // }
                        //     let depth = (op - 0x90 + 1) as i32;
                        //     let off1 = offset - 32;
                        //     let off2 = offset - ((depth + 1) * 32);

                        //     let ptr1 = builder.ins().iadd_imm(stack_cursor, off1 as i64);
                        //     let ptr2 = builder.ins().iadd_imm(stack_cursor, off2 as i64);
                        //     let mem = MemFlags::new();

                        //     // Swap Low 128 bits
                        //     let v1_lo = builder.ins().load(types::I128, mem, ptr1, 0);
                        //     let v2_lo = builder.ins().load(types::I128, mem, ptr2, 0);
                        //     builder.ins().store(mem, v2_lo, ptr1, 0);
                        //     builder.ins().store(mem, v1_lo, ptr2, 0);

                        //     // Swap High 128 bits
                        //     let v1_hi = builder.ins().load(types::I128, mem, ptr1, 16);
                        //     let v2_hi = builder.ins().load(types::I128, mem, ptr2, 16);
                        //     builder.ins().store(mem, v2_hi, ptr1, 16);
                        //     builder.ins().store(mem, v1_hi, ptr2, 16);
                        //     // println!("[{:#x}]({}): jit_swapN", current_real_pc, offset);
                        // }

                        // // -------------------------------------------------------------
                        // // [Group 5] Flow Control
                        // // -------------------------------------------------------------
                        // // JUMP, JUMPI: Go Analyzer가 이미 "여기서 끊어라"고 했으므로
                        // // 실제 점프 로직은 Go가 처리함.
                        // // JIT은 스택만 맞춰주고(Pop) 종료하면 됨.

                        // // 0x56(JUMP): Pop 1
                        // 0x56 => {
                        //     // unreachable!("AAA-1");
                        //     offset -= 32;
                        // }

                        // // 0x57(JUMPI): Pop 2
                        // 0x57 => {
                        //     // unreachable!("AAA-2");
                        //     offset -= 64;
                        // }

                        // // 0x58(PC): Push 1
                        // 0x58 => {
                        //     // unimplemented!("AAA");
                        //     let val = current_real_pc;
                        //     let ptr = builder.ins().iadd_imm(stack_cursor, offset as i64);
                        //     let v_pc = builder.ins().iconst(types::I64, val as i64);
                        //     let mem = MemFlags::new();
                        //     // u64로 저장
                        //     builder.ins().store(mem, v_pc, ptr, 0);
                        //     // 나머지 0 채우기
                        //     let zero = builder.ins().iconst(types::I64, 0);
                        //     for i in 1..4 {
                        //         builder.ins().store(mem, zero, ptr, i * 8);
                        //     }
                        //     offset += 32;
                        // }

                        // // 0x5b(JUMPDEST): No-op
                        // 0x5b => {
                        //     // println!("[{:#x}]({}): JUMPDEST", current_real_pc, offset);
                        // }

                        // _ => {
                        //     unreachable!("{}", format!("unsupportd instruction: {:?}", op));
                        //     // 미지원 Opcode는 Go Analyzer가 걸러냈으므로 여기 올 일 없음.
                        // }
            }
        }

        // let v1 = builder.ins().iconst(types::I32, 1);
        // let v2 = builder.ins().iconst(types::I32, 2);
        // builder.ins().iadd(v1, v2);

        // let v3 = builder.ins().iconst(types::I32, 3);
        // let v4 = builder.ins().iconst(types::I32, 4);
        // builder.ins().iadd(v3, v4);

        builder.ins().return_(&[]);
        builder.finalize();
        // println!("AAA {}", cnt);
    }

    engine.counter += 1;

    //     {
    //         let func_name = format!("trace_{}", engine.counter);
    //         dump_cranelift_ir(&ctx.func, &func_name);
    //     }

    let id = engine
        .module
        .declare_function(
            &format!("trace_{}", engine.counter),
            Linkage::Export,
            &ctx.func.signature,
        )
        .unwrap();
    engine.module.define_function(id, ctx).unwrap();
    let code_len = ctx.compiled_code().unwrap().buffer.data().len();
    engine.module.clear_context(ctx);
    engine.module.finalize_definitions().unwrap();

    //     {
    //         let code_ptr = engine.module.get_finalized_function(id);
    //         dump::dump_machine_code(code_ptr, code_len);
    //     }
    engine.module.get_finalized_function(id)
}

// 3. 실행 함수
#[no_mangle]
pub extern "C" fn execute_jit_func(
    ptr: *const u8,
    stack_cursor: *mut u64,
    input_ptr: *const u8,
    input_len: u64,
) {
    unsafe {
        let func = std::mem::transmute::<_, extern "C" fn(*mut u64, *const u8, u64)>(ptr);
        func(stack_cursor, input_ptr, input_len);
    }
}

// [Helper] Cranelift IR(CLIF)을 출력하는 함수
fn dump_cranelift_ir(func: &codegen::ir::Function, name: &str) {
    println!("\n=== [Cranelift IR Dump: {}] ===", name);
    // Function 객체는 Display를 구현하고 있어 바로 출력 가능
    println!("{}", func.display());
    println!("=== [End of IR] ===\n");
}
// =============================================================================
// [3] Inline Optimizer Logic (Stateless)
// =============================================================================

// 필드 없음 (상태를 가지지 않음)
struct InlineOps;

impl InlineOps {
    // -------------------------------------------------------------------------
    // [Arithmetic] builder를 인자로 받음
    // -------------------------------------------------------------------------
    fn u256_add(builder: &mut FunctionBuilder, a: [Value; 4], b: [Value; 4]) -> [Value; 4] {
        let mut res = [builder.ins().iconst(types::I64, 0); 4];
        let mut carry: Option<Value> = None;

        for i in 0..4 {
            let sum_ab = builder.ins().iadd(a[i], b[i]);
            let carry_ab = builder.ins().icmp(IntCC::UnsignedLessThan, sum_ab, a[i]);
            let carry_ab_i64 = builder.ins().uextend(types::I64, carry_ab);

            if let Some(c) = carry {
                let sum_total = builder.ins().iadd(sum_ab, c);
                let carry_c = builder
                    .ins()
                    .icmp(IntCC::UnsignedLessThan, sum_total, sum_ab);
                let carry_c_i64 = builder.ins().uextend(types::I64, carry_c);

                let next_c = builder.ins().bor(carry_ab_i64, carry_c_i64);
                res[i] = sum_total;
                carry = Some(next_c);
            } else {
                res[i] = sum_ab;
                carry = Some(carry_ab_i64);
            }
        }
        res
    }

    fn u256_sub(builder: &mut FunctionBuilder, a: [Value; 4], b: [Value; 4]) -> [Value; 4] {
        let mut res = [builder.ins().iconst(types::I64, 0); 4];
        let mut borrow: Option<Value> = None;

        for i in 0..4 {
            let diff_ab = builder.ins().isub(a[i], b[i]);
            let borrow_ab = builder.ins().icmp(IntCC::UnsignedLessThan, a[i], b[i]);
            let borrow_ab_i64 = builder.ins().uextend(types::I64, borrow_ab);

            if let Some(b_val) = borrow {
                let diff_total = builder.ins().isub(diff_ab, b_val);
                let borrow_c = builder.ins().icmp(IntCC::UnsignedLessThan, diff_ab, b_val);
                let borrow_c_i64 = builder.ins().uextend(types::I64, borrow_c);

                let next_b = builder.ins().bor(borrow_ab_i64, borrow_c_i64);
                res[i] = diff_total;
                borrow = Some(next_b);
            } else {
                res[i] = diff_ab;
                borrow = Some(borrow_ab_i64);
            }
        }
        res
    }

    // -------------------------------------------------------------------------
    // [Bitwise]
    // -------------------------------------------------------------------------
    fn u256_bitwise_128(
        builder: &mut FunctionBuilder,
        a: [Value; 2],
        b: [Value; 2],
        op: fn(&mut FunctionBuilder, Value, Value) -> Value,
    ) -> [Value; 2] {
        [op(builder, a[0], b[0]), op(builder, a[1], b[1])]
    }

    fn u256_not_128(builder: &mut FunctionBuilder, a: [Value; 2]) -> [Value; 2] {
        [builder.ins().bnot(a[0]), builder.ins().bnot(a[1])]
    }

    // -------------------------------------------------------------------------
    // [Comparison]
    // -------------------------------------------------------------------------
    fn u256_cmp(
        builder: &mut FunctionBuilder,
        a: [Value; 4],
        b: [Value; 4],
        cc: IntCC,
    ) -> [Value; 4] {
        let one = builder.ins().iconst(types::I64, 1);
        let zero = builder.ins().iconst(types::I64, 0);
        let mut result = zero;

        for i in 0..4 {
            let is_diff = builder.ins().icmp(IntCC::NotEqual, a[i], b[i]);
            let is_true = builder.ins().icmp(cc, a[i], b[i]);
            let val_if_true = one;
            let local_res = builder.ins().select(is_true, val_if_true, zero);
            result = builder.ins().select(is_diff, local_res, result);
        }
        [result, zero, zero, zero]
    }

    fn u256_eq(builder: &mut FunctionBuilder, a: [Value; 4], b: [Value; 4]) -> [Value; 4] {
        let mut acc = builder.ins().iconst(types::I64, 1);
        for i in 0..4 {
            let eq = builder.ins().icmp(IntCC::Equal, a[i], b[i]);
            let eq_i64 = builder.ins().uextend(types::I64, eq);
            acc = builder.ins().band(acc, eq_i64);
        }
        let zero = builder.ins().iconst(types::I64, 0);
        [acc, zero, zero, zero]
    }

    fn u256_iszero(builder: &mut FunctionBuilder, a: [Value; 4]) -> [Value; 4] {
        let v01 = builder.ins().bor(a[0], a[1]);
        let v23 = builder.ins().bor(a[2], a[3]);
        let all = builder.ins().bor(v01, v23);
        let is_zero = builder.ins().icmp_imm(IntCC::Equal, all, 0);
        let res = builder.ins().uextend(types::I64, is_zero);
        let zero = builder.ins().iconst(types::I64, 0);
        [res, zero, zero, zero]
    }
}

// Runtime 함수 Import Helper
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

    // 인자는 모두 포인터(i64)로 통일
    for _ in 0..num_args {
        sig.params.push(AbiParam::new(types::I64));
    }
    // 리턴값 없음 (void)

    let id = module
        .declare_function(name, Linkage::Import, &sig)
        .unwrap();
    funcs.insert(name.to_string(), id);
    id
}
