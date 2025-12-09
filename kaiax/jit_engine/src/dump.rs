use capstone::prelude::*; // 상단에 추가
use std::slice;

// [Helper] 기계어(Machine Code)를 어셈블리로 덤프하는 함수
pub fn dump_machine_code(code_ptr: *const u8, len: usize) {
    println!("\n=== [Machine Code Dump: {} bytes] ===", len);

    // 1. 아키텍처에 맞는 Capstone 엔진 생성
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

    // 2. Unsafe Pointer -> Rust Slice 변환
    let code_slice = unsafe { slice::from_raw_parts(code_ptr, len) };

    // 3. 디스어셈블 및 출력
    // code_ptr을 시작 주소(Base Address)로 설정하여 상대 주소 계산을 도움
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
