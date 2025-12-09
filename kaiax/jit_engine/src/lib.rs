mod dump;
mod engine;
mod runtime;

use std::ptr;

use cranelift::codegen::Context; // [Fix 1] Context 타입 명시적 import
use cranelift::prelude::*;
use cranelift_jit::{JITBuilder, JITModule};
use cranelift_module::{Linkage, Module};

// // 전역 상태를 유지할 구조체
// pub struct JitEngine {
//     module: JITModule,
//     ctx: Context,
//     func_ctx: FunctionBuilderContext,
//     counter: u32,
// }

// #[no_mangle]
// pub extern "C" fn new_jit_engine() -> *mut JitEngine {
//     let mut flag_builder = settings::builder();
//     flag_builder.set("use_colocated_libcalls", "false").unwrap();
//     flag_builder.set("is_pic", "true").unwrap();

//     // [Fix 2] cranelift-native 크레이트가 있어야 이 함수 사용 가능
//     let isa_builder = cranelift_native::builder().unwrap_or_else(|msg| {
//         panic!("host machine is not supported: {}", msg);
//     });
//     let isa = isa_builder
//         .finish(settings::Flags::new(flag_builder))
//         .unwrap();

//     let builder = JITBuilder::with_isa(isa, cranelift_module::default_libcall_names());

//     // [Fix 3] 구조체 안에서 module.make_context()를 바로 호출하면 에러남.
//     // 변수로 먼저 분리해서 순서대로 생성해야 함.
//     let mut module = JITModule::new(builder);
//     let ctx = module.make_context();
//     let func_ctx = FunctionBuilderContext::new();

//     let engine = JitEngine {
//         module, // 위에서 만든 변수 소유권 이동
//         ctx,    // 위에서 만든 변수 소유권 이동
//         func_ctx,
//         counter: 0,
//     };

//     Box::into_raw(Box::new(engine))
// }

// #[no_mangle]
// pub extern "C" fn compile_add_func(ptr: *mut JitEngine) -> *const u8 {
//     let engine = unsafe {
//         assert!(!ptr.is_null());
//         &mut *ptr
//     };

//     let ctx = &mut engine.ctx;
//     let func_ctx = &mut engine.func_ctx;
//     let module = &mut engine.module;

//     module.clear_context(ctx);

//     ctx.func.signature.params.push(AbiParam::new(types::I64));
//     ctx.func.signature.params.push(AbiParam::new(types::I64));
//     ctx.func.signature.returns.push(AbiParam::new(types::I64));

//     {
//         let mut bcx = FunctionBuilder::new(&mut ctx.func, func_ctx);
//         let block = bcx.create_block();
//         bcx.switch_to_block(block);
//         bcx.append_block_params_for_function_params(block);

//         let a = bcx.block_params(block)[0];
//         let b = bcx.block_params(block)[1];
//         let sum = bcx.ins().iadd(a, b);

//         bcx.ins().return_(&[sum]);
//         bcx.seal_all_blocks();
//         bcx.finalize();
//     }

//     engine.counter += 1;
//     // 함수 이름이 중복되면 Cranelift 내부에서 패닉이 날 수 있으므로 유니크하게 생성
//     let func_name = format!("jit_func_{}", engine.counter);

//     let id = module
//         .declare_function(&func_name, Linkage::Export, &ctx.func.signature)
//         .unwrap();
//     module.define_function(id, ctx).unwrap();
//     module.finalize_definitions().unwrap();

//     let code_ptr = module.get_finalized_function(id);
//     code_ptr
// }

// #[no_mangle]
// pub extern "C" fn free_jit_engine(ptr: *mut JitEngine) {
//     if ptr.is_null() {
//         return;
//     }
//     unsafe {
//         let _ = Box::from_raw(ptr);
//     }
// }

// #[no_mangle]
// pub extern "C" fn compile_trajactory(
//     // <-- 여기 이름 확인!
//     ptr: *mut JitEngine,
//     code_ptr: *const u8,
//     len: usize,
// ) -> *const u8 {
//     return ptr::null();
// }

#[no_mangle]
pub extern "C" fn test123(ptr: *const u8, stack_cursor: *mut u64) {}
