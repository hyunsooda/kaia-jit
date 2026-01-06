use capstone::prelude::*; // 상단에 추가
use cranelift::codegen;
use std::slice;

pub fn dump_machine_code(code_ptr: *const u8, len: usize) {
    println!("\n=== [Machine Code Dump: {} bytes] ===", len);

    #[cfg(target_arch = "x86_64")]
    let cs = Capstone::new()
        .x86()
        .mode(arch::x86::ArchMode::Mode64)
        .syntax(arch::x86::ArchSyntax::Intel) // Intel 문법 (dest, src)
        .build()
        .expect("Failed to create Capstone for x86_64");

    #[cfg(target_arch = "aarch64")]
    let cs = Capstone::new()
        .arm64()
        .mode(arch::arm64::ArchMode::Arm)
        .build()
        .expect("Failed to create Capstone for ARM64");

    let code_slice = unsafe { slice::from_raw_parts(code_ptr, len) };

    match cs.disasm_all(code_slice, code_ptr as u64) {
        Ok(insns) => {
            for i in insns.iter() {
                println!(
                    "0x{:x}:\t{:6}\t{}",
                    i.address(),
                    i.mnemonic().unwrap_or(""),
                    i.op_str().unwrap_or("")
                );
            }
        }
        Err(e) => {
            panic!("{:?}", e);
        }
    }
    println!("=== [End of Machine Code] ===\n");
}

pub fn dump_cranelift_ir(func: &codegen::ir::Function, name: &str) {
    println!("\n=== [Cranelift IR Dump: {}] ===", name);
    // Function 객체는 Display를 구현하고 있어 바로 출력 가능
    println!("{}", func.display());
    println!("=== [End of IR] ===\n");
}
