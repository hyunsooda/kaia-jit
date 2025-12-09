#include "textflag.h"

// Go 함수 선언:
// func Execute(codePtr unsafe.Pointer, stackCursor unsafe.Pointer, inputPtr unsafe.Pointer, inputLen uint64)
// 인자 크기 총합: 8 * 4 = 32바이트
// 따라서 $0-16 이 아니라 $0-32 로 변경해야 합니다.
TEXT ·Execute(SB), NOSPLIT, $0-32
    // 1. 스택 프레임 백업
    PUSHQ BP
    MOVQ SP, BP

    // 2. 인자 로드 (System V ABI 호출 규약 준수)
    
    // [Target Address] codePtr (Go stack offset 0) -> R10
    MOVQ codePtr+0(FP), R10

    // [Arg 1] stackCursor (Go stack offset 8) -> RDI
    MOVQ stackCursor+8(FP), DI

    // [Arg 2] inputPtr (Go stack offset 16) -> RSI
    // 2번째 인자는 RSI 레지스터를 사용합니다.
    MOVQ inputPtr+16(FP), SI

    // [Arg 3] inputLen (Go stack offset 24) -> RDX
    // 3번째 인자는 RDX 레지스터를 사용합니다.
    MOVQ inputLen+24(FP), DX

    // 3. 스택 확장 및 정렬 (Rust/C 호환성 및 안전지대 확보)
    SUBQ $512, SP
    ANDQ $-16, SP

    // 4. JIT 함수 호출
    // fn(stack_cursor, input_ptr, input_len)
    CALL R10

    // 5. 스택 복구 및 리턴
    MOVQ BP, SP
    POPQ BP
    RET
