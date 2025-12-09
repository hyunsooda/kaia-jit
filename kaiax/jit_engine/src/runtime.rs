use ethnum::{I256, U256};
use std::slice;

// ============================================================================
// [ Arithmetic Operations ]
// ============================================================================

#[no_mangle]
pub unsafe extern "C" fn jit_add(ptr_result_and_second: *mut U256, ptr_top: *const U256) {
    // ADD: Stack[0] + Stack[1]
    let top = *ptr_top;
    let second = *ptr_result_and_second;

    // 결과는 두 번째 위치(Top-1)에 덮어씀
    *ptr_result_and_second = top.wrapping_add(second);
}

#[no_mangle]
pub unsafe extern "C" fn jit_mul(ptr_result_and_second: *mut U256, ptr_top: *const U256) {
    // MUL: Stack[0] * Stack[1]
    let top = *ptr_top;
    let second = *ptr_result_and_second;

    *ptr_result_and_second = top.wrapping_mul(second);
}

#[no_mangle]
pub unsafe extern "C" fn jit_sub(ptr_result_and_second: *mut U256, ptr_top: *const U256) {
    // SUB: Stack[0] - Stack[1]
    // (EVM Spec: first_item - second_item)
    let top = *ptr_top;
    let second = *ptr_result_and_second;

    *ptr_result_and_second = top.wrapping_sub(second);
}

#[no_mangle]
pub unsafe extern "C" fn jit_div(ptr_result_and_second: *mut U256, ptr_top: *const U256) {
    // DIV: Stack[0] / Stack[1]
    // (EVM Spec: numerator / denominator)
    let numerator = *ptr_top; // 분자
    let denominator = *ptr_result_and_second; // 분모

    if denominator == U256::ZERO {
        *ptr_result_and_second = U256::ZERO;
    } else {
        *ptr_result_and_second = numerator / denominator;
    }
}

#[no_mangle]
pub unsafe extern "C" fn jit_sdiv(ptr_result_and_second: *mut I256, ptr_top: *const I256) {
    // SDIV: Stack[0] / Stack[1] (Signed)
    let numerator = *ptr_top;
    let denominator = *ptr_result_and_second;

    if denominator == 0 {
        *ptr_result_and_second = I256::ZERO;
    } else if numerator == I256::MIN && denominator == -1 {
        // EVM 특수 룰: MIN / -1 = MIN (Overflow 방지)
        *ptr_result_and_second = I256::MIN;
    } else {
        *ptr_result_and_second = numerator / denominator;
    }
}

#[no_mangle]
pub unsafe extern "C" fn jit_mod(ptr_result_and_second: *mut U256, ptr_top: *const U256) {
    // MOD: Stack[0] % Stack[1]
    let numerator = *ptr_top;
    let denominator = *ptr_result_and_second;

    if denominator == U256::ZERO {
        *ptr_result_and_second = U256::ZERO;
    } else {
        *ptr_result_and_second = numerator % denominator;
    }
}

#[no_mangle]
pub unsafe extern "C" fn jit_smod(ptr_result_and_second: *mut I256, ptr_top: *const I256) {
    // SMOD: Stack[0] % Stack[1] (Signed)
    let numerator = *ptr_top;
    let denominator = *ptr_result_and_second;

    if denominator == 0 {
        *ptr_result_and_second = I256::ZERO;
    } else {
        *ptr_result_and_second = numerator % denominator;
    }
}

#[no_mangle]
pub unsafe extern "C" fn jit_signextend(ptr_result_and_second: *mut U256, ptr_top: *const U256) {
    // SIGNEXTEND(byte_num, value)
    // Stack[0]: byte_num (확장할 바이트 위치)
    // Stack[1]: value    (값)
    let byte_num = *ptr_top;
    let value = *ptr_result_and_second;

    if byte_num < 31 {
        let bit_pos = (byte_num.as_u32() * 8) + 7;
        let mask = (U256::ONE << bit_pos) - 1;
        let sign_bit = (value >> bit_pos) & 1;

        if sign_bit == 1 {
            *ptr_result_and_second = value | !mask;
        } else {
            *ptr_result_and_second = value & mask;
        }
    } else {
        // 31바이트 이상이면 값 변화 없음
        *ptr_result_and_second = value;
    }
}

// ============================================================================
// [ Ternary Operations (인자 3개) ]
// ============================================================================
// 순서: Stack[0] (Top), Stack[1] (Mid), Stack[2] (Bottom/Result)

#[no_mangle]
pub unsafe extern "C" fn jit_addmod(
    ptr_result_and_third: *mut U256,
    ptr_second: *const U256,
    ptr_top: *const U256,
) {
    // ADDMOD(a, b, N) -> (a + b) % N
    // Stack: [a, b, N] -> a=Top, b=Second, N=Third
    let a = *ptr_top;
    let b = *ptr_second;
    let n = *ptr_result_and_third;

    if n == U256::ZERO {
        *ptr_result_and_third = U256::ZERO;
    } else {
        // U512 확장 계산 필요 (ethnum U256은 overflowing_add만 지원하므로 로직 구현 필요)
        // 편의상 U256 Wrapping으로 구현 (Overflow 시 틀릴 수 있음 -> U512 라이브러리 사용 권장)
        // 여기서는 프로토타입이므로 wrapping 로직 사용
        let sum = a.wrapping_add(b);
        *ptr_result_and_third = sum % n;
    }
}

#[no_mangle]
pub unsafe extern "C" fn jit_mulmod(
    ptr_result_and_third: *mut U256,
    ptr_second: *const U256,
    ptr_top: *const U256,
) {
    // MULMOD(a, b, N) -> (a * b) % N
    let a = *ptr_top;
    let b = *ptr_second;
    let n = *ptr_result_and_third;

    if n == U256::ZERO {
        *ptr_result_and_third = U256::ZERO;
    } else {
        let prod = a.wrapping_mul(b);
        *ptr_result_and_third = prod % n;
    }
}

// ============================================================================
// [ Comparison Operations ]
// ============================================================================

#[no_mangle]
pub unsafe extern "C" fn jit_lt(ptr_result_and_second: *mut U256, ptr_top: *const U256) {
    // LT: Stack[0] < Stack[1]
    let top = *ptr_top;
    let second = *ptr_result_and_second;

    *ptr_result_and_second = if top < second { U256::ONE } else { U256::ZERO };
}

#[no_mangle]
pub unsafe extern "C" fn jit_gt(ptr_result_and_second: *mut U256, ptr_top: *const U256) {
    // GT: Stack[0] > Stack[1]
    let top = *ptr_top;
    let second = *ptr_result_and_second;

    *ptr_result_and_second = if top > second { U256::ONE } else { U256::ZERO };
}

#[no_mangle]
pub unsafe extern "C" fn jit_slt(ptr_result_and_second: *mut I256, ptr_top: *const I256) {
    let top = *ptr_top;
    let second = *ptr_result_and_second;

    // boolean true=1, false=0
    *ptr_result_and_second = if top < second { I256::ONE } else { I256::ZERO };
}

#[no_mangle]
pub unsafe extern "C" fn jit_sgt(ptr_result_and_second: *mut I256, ptr_top: *const I256) {
    let top = *ptr_top;
    let second = *ptr_result_and_second;

    *ptr_result_and_second = if top > second { I256::ONE } else { I256::ZERO };
}

#[no_mangle]
pub unsafe extern "C" fn jit_eq(ptr_result_and_second: *mut U256, ptr_top: *const U256) {
    let top = *ptr_top;
    let second = *ptr_result_and_second;

    *ptr_result_and_second = if top == second { U256::ONE } else { U256::ZERO };
}

#[no_mangle]
pub unsafe extern "C" fn jit_iszero(ptr_top: *mut U256) {
    // ISZERO: Stack[0] == 0
    // (인자가 1개이므로 ptr_top 위치에 바로 덮어씀)
    let top = *ptr_top;
    *ptr_top = if top == U256::ZERO {
        U256::ONE
    } else {
        U256::ZERO
    };
}

// ============================================================================
// [ Bitwise & Shift Operations ]
// ============================================================================

#[no_mangle]
pub unsafe extern "C" fn jit_and(ptr_result_and_second: *mut U256, ptr_top: *const U256) {
    let top = *ptr_top;
    *ptr_result_and_second &= top;
}

#[no_mangle]
pub unsafe extern "C" fn jit_or(ptr_result_and_second: *mut U256, ptr_top: *const U256) {
    let top = *ptr_top;
    *ptr_result_and_second |= top;
}

#[no_mangle]
pub unsafe extern "C" fn jit_xor(ptr_result_and_second: *mut U256, ptr_top: *const U256) {
    let top = *ptr_top;
    *ptr_result_and_second ^= top;
}

#[no_mangle]
pub unsafe extern "C" fn jit_not(ptr_top: *mut U256) {
    // NOT: ~Stack[0]
    *ptr_top = !(*ptr_top);
}

#[no_mangle]
pub unsafe extern "C" fn jit_byte(ptr_result_and_second: *mut U256, ptr_top: *const U256) {
    // BYTE(offset, value)
    // Stack[0]: offset
    // Stack[1]: value
    let offset = *ptr_top;
    let value = *ptr_result_and_second;

    if offset > 31 {
        *ptr_result_and_second = U256::ZERO;
    } else {
        // EVM은 Big Endian이므로, offset 0은 가장 왼쪽(MSB) 바이트임
        let shift = (31 - offset.as_u32()) * 8;
        *ptr_result_and_second = (value >> shift) & 0xff;
    }
}

#[no_mangle]
pub unsafe extern "C" fn jit_shl(ptr_result_and_second: *mut U256, ptr_top: *const U256) {
    // SHL(shift, value)
    // Stack[0]: shift
    // Stack[1]: value
    let shift = *ptr_top;
    let value = *ptr_result_and_second;

    if shift >= 256 {
        *ptr_result_and_second = U256::ZERO;
    } else {
        *ptr_result_and_second = value << shift.as_u32();
    }
}

#[no_mangle]
pub unsafe extern "C" fn jit_shr(ptr_result_and_second: *mut U256, ptr_top: *const U256) {
    // SHR(shift, value)
    let shift = *ptr_top;
    let value = *ptr_result_and_second;

    if shift >= 256 {
        *ptr_result_and_second = U256::ZERO;
    } else {
        *ptr_result_and_second = value >> shift.as_u32();
    }
}

#[no_mangle]
pub unsafe extern "C" fn jit_sar(ptr_result_and_second: *mut I256, ptr_top: *const I256) {
    // SAR(shift, value)
    let shift_signed = *ptr_top; // shift amount as signed int
    let value = *ptr_result_and_second;

    // shift 크기는 unsigned로 해석해야 함 (음수 shift는 없음)
    // ethnum I256 -> u256 변환 후 체크
    let shift_u256: U256 = std::mem::transmute(shift_signed);

    if shift_u256 >= 256 {
        // 부호 유지하며 다 밀어버림
        if value < 0 {
            *ptr_result_and_second = I256::new(-1); // 111...111
        } else {
            *ptr_result_and_second = I256::ZERO;
        }
    } else {
        *ptr_result_and_second = value >> shift_u256.as_u32();
    }
}

// TODO: inline me
#[no_mangle]
pub extern "C" fn jit_calldataload(stack_ptr: *mut u64, input_ptr: *const u8, input_len: u64) {
    unsafe {
        // [1] 스택에서 Offset 확인 (Little Endian Layout)
        // stack_ptr[0]이 LSB(Low 64bit)입니다.
        let offset = *stack_ptr.add(0);

        // 상위 192비트(stack[1], stack[2], stack[3]) 중 하나라도 0이 아니면
        // Offset이 2^64보다 크다는 뜻이므로 무조건 Out of Bounds입니다.
        let huge_offset = (*stack_ptr.add(1) | *stack_ptr.add(2) | *stack_ptr.add(3)) != 0;

        // [2] 범위 체크 (EVM Logic)
        // - 오프셋이 너무 크거나(huge_offset)
        // - 오프셋이 입력 길이보다 크거나 같으면(offset >= input_len)
        // -> 결과는 0입니다.
        if huge_offset || offset >= input_len {
            // 32바이트 0으로 채움
            // (memset처럼 0을 4번 쓰는 게 가장 빠릅니다)
            *stack_ptr.add(0) = 0;
            *stack_ptr.add(1) = 0;
            *stack_ptr.add(2) = 0;
            *stack_ptr.add(3) = 0;
            return;
        }

        // [3] 복사할 길이 계산 (이제 뺄셈 안전함)
        // 위에서 offset < input_len임이 보장되었으므로 뺄셈 결과는 양수입니다.
        let remaining = input_len - offset;

        // 32바이트보다 많이 남았으면 32바이트만, 적게 남았으면 남은 만큼만
        let copy_len = if remaining > 32 {
            32
        } else {
            remaining as usize
        };

        // [4] 버퍼 준비 및 복사
        // 0으로 초기화된 버퍼 생성 (Padding 자동 처리)
        let mut buf = [0u8; 32];

        // 입력 데이터에서 copy_len만큼만 복사
        let src_slice = slice::from_raw_parts(input_ptr.add(offset as usize), copy_len);
        buf[0..copy_len].copy_from_slice(src_slice);

        // [5] 결과 저장 (Native Endian)
        // BigEndian Data -> Native U256 -> Stack
        let val = ethnum::U256::from_be_bytes(buf);
        *(stack_ptr as *mut ethnum::U256) = val;
    }
}
