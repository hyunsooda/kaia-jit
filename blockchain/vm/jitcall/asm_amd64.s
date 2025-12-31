#include "textflag.h"

// Go 함수 선언:
// func Execute(codePtr, stackCursor, memoryPtr, inputPtr, inputLen)
// 인자 크기 총합: 8바이트 * 5개 = 40바이트
// $0-32 -> $0-40 으로 변경
TEXT ·Execute(SB), NOSPLIT, $0-40
    // 1. 스택 프레임 백업
    PUSHQ BP
    MOVQ SP, BP

    // 2. 인자 로드 (System V ABI: RDI, RSI, RDX, RCX, R8, R9)

    // [Target Address] codePtr (offset 0) -> R10 (호출할 주소)
    MOVQ codePtr+0(FP), R10

    // [Arg 1] stackCursor (offset 8) -> RDI
    MOVQ stackCursor+8(FP), DI

    // [Arg 2] memoryPtr (offset 16) -> RSI
    // 새로 추가된 인자입니다. 2번째 인자는 RSI를 사용합니다.
    MOVQ memoryPtr+16(FP), SI

    // [Arg 3] inputPtr (offset 24) -> RDX
    // 기존 2번째에서 3번째로 밀려났으므로 RDX를 사용합니다.
    MOVQ inputPtr+24(FP), DX

    // [Arg 4] inputLen (offset 32) -> RCX
    // 기존 3번째에서 4번째로 밀려났으므로 RCX를 사용합니다.
    MOVQ inputLen+32(FP), CX

    // 3. 스택 확장 및 정렬 (16바이트 정렬 보장)
    SUBQ $512, SP
    ANDQ $-16, SP

    // 4. JIT 함수 호출
    // fn(stack_cursor, memory_ptr, input_ptr, input_len)
    CALL R10

    // 5. 스택 복구 및 리턴
    MOVQ BP, SP
    POPQ BP
    RET
