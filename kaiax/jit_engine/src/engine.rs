use crate::runtime;
use cranelift::prelude::{isa::TargetIsa, *};
use cranelift_jit::{JITBuilder, JITModule};
use cranelift_module::{FuncId, Module};
use std::os::raw::c_char;
use std::sync::Arc;
use std::{collections::HashMap, ffi::CString};

pub struct JitEngine {
    pub builder_context: FunctionBuilderContext,
    pub ctx: codegen::Context,
    pub module: JITModule,
    pub counter: u32,
    pub funcs: HashMap<String, FuncId>,
    pub isa: Arc<dyn TargetIsa>,
}

#[no_mangle]
pub extern "C" fn new_jit_engine() -> *mut JitEngine {
    let mut flag_builder = settings::builder();
    flag_builder.set("use_colocated_libcalls", "false").unwrap();
    // As we're statically linking the JIT binary from Kaia(Golang), setting the PIC `false` is
    // safe
    flag_builder.set("is_pic", "false").unwrap();

    let isa_builder = cranelift_native::builder().unwrap_or_else(|msg| {
        panic!("host machine is not supported: {}", msg);
    });
    let isa = isa_builder
        .finish(settings::Flags::new(flag_builder))
        .unwrap();
    let mut builder = JITBuilder::with_isa(isa.clone(), cranelift_module::default_libcall_names());

    macro_rules! register {
        ($name:expr, $func:ident) => {
            builder.symbol($name, runtime::$func as *const u8);
        };
    }

    // register!("jit_debug_print", jit_debug_print);
    register!("jit_mul", jit_mul);
    register!("jit_div", jit_div);
    register!("jit_sdiv", jit_sdiv);
    register!("jit_mod", jit_mod);
    register!("jit_smod", jit_smod);
    register!("jit_addmod", jit_addmod);
    register!("jit_mulmod", jit_mulmod);
    register!("jit_signextend", jit_signextend);
    register!("jit_sha3", jit_sha3);
    register!("jit_byte", jit_byte);
    register!("jit_calldataload", jit_calldataload);

    let module = JITModule::new(builder);
    let engine = JitEngine {
        builder_context: FunctionBuilderContext::new(),
        ctx: module.make_context(),
        module,
        counter: 0,
        funcs: HashMap::new(),
        isa,
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

#[no_mangle]
pub extern "C" fn free_error_msg(s: *mut c_char) {
    if s.is_null() {
        return;
    }
    unsafe {
        let _ = CString::from_raw(s);
    }
}

#[no_mangle]
pub extern "C" fn execute_jit_func(
    ptr: *const u8,
    stack_cursor: *mut u64,
    mem_ptr: *mut u64,
    mem_len: u64,
    input_ptr: *const u8,
    input_len: u64,
) {
    unsafe {
        let func =
            std::mem::transmute::<_, extern "C" fn(*mut u64, *mut u64, u64, *const u8, u64)>(ptr);
        func(stack_cursor, mem_ptr, mem_len, input_ptr, input_len);
    }
}
