use cranelift::prelude::*;

// Stateless
pub struct InlineOps;

impl InlineOps {
    // -------------------------------------------------------------------------
    // [Arithmetic] builder를 인자로 받음
    // -------------------------------------------------------------------------
    pub fn u256_add(builder: &mut FunctionBuilder, a: [Value; 4], b: [Value; 4]) -> [Value; 4] {
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

    pub fn u256_sub(builder: &mut FunctionBuilder, a: [Value; 4], b: [Value; 4]) -> [Value; 4] {
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
    pub fn u256_bitwise_128(
        builder: &mut FunctionBuilder,
        a: [Value; 2],
        b: [Value; 2],
        op: fn(&mut FunctionBuilder, Value, Value) -> Value,
    ) -> [Value; 2] {
        [op(builder, a[0], b[0]), op(builder, a[1], b[1])]
    }

    pub fn u256_not_128(builder: &mut FunctionBuilder, a: [Value; 2]) -> [Value; 2] {
        [builder.ins().bnot(a[0]), builder.ins().bnot(a[1])]
    }

    // -------------------------------------------------------------------------
    // [Comparison]
    // -------------------------------------------------------------------------
    pub fn u256_cmp(
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

    pub fn u256_eq(builder: &mut FunctionBuilder, a: [Value; 4], b: [Value; 4]) -> [Value; 4] {
        let mut acc = builder.ins().iconst(types::I64, 1);
        for i in 0..4 {
            let eq = builder.ins().icmp(IntCC::Equal, a[i], b[i]);
            let eq_i64 = builder.ins().uextend(types::I64, eq);
            acc = builder.ins().band(acc, eq_i64);
        }
        let zero = builder.ins().iconst(types::I64, 0);
        [acc, zero, zero, zero]
    }

    pub fn u256_iszero(builder: &mut FunctionBuilder, a: [Value; 4]) -> [Value; 4] {
        let v01 = builder.ins().bor(a[0], a[1]);
        let v23 = builder.ins().bor(a[2], a[3]);
        let all = builder.ins().bor(v01, v23);
        let is_zero = builder.ins().icmp_imm(IntCC::Equal, all, 0);
        let res = builder.ins().uextend(types::I64, is_zero);
        let zero = builder.ins().iconst(types::I64, 0);
        [res, zero, zero, zero]
    }

    // SHL (Shift Left)
    pub fn u256_shl(
        builder: &mut FunctionBuilder,
        shift: [Value; 4],
        val: [Value; 4],
    ) -> [Value; 4] {
        let zero = builder.ins().iconst(types::I64, 0);

        // 1. Check for Oversized Shift (Shift >= 256)
        // If shift is larger than 255, the result is 0.
        // We must check:
        // A. Are any high words (shift[1], shift[2], shift[3]) non-zero?
        //    If yes, the shift amount is >= 2^64, which is definitely > 256.
        // B. Is the low word (shift[0]) >= 256?
        let high_words = builder.ins().bor(shift[1], shift[2]);
        let high_nonzero = builder.ins().bor(high_words, shift[3]);
        let low_large = builder
            .ins()
            .icmp_imm(IntCC::UnsignedGreaterThanOrEqual, shift[0], 256);
        let any_high = builder.ins().icmp_imm(IntCC::NotEqual, high_nonzero, 0);

        // Combine conditions: oversized if (low >= 256) OR (any high word is set)
        let is_oversized = builder.ins().bor(low_large, any_high);

        // Actual shift amount (taken from the lowest 64 bits)
        let amount = shift[0];

        // 2. Word Shuffle (Coarse Shift by 64-bit chunks)
        // Calculate how many full 64-bit words to shift: amount / 64
        let word_sh = builder.ins().ushr_imm(amount, 6); // amount >> 6
        let word_sh_masked = builder.ins().band_imm(word_sh, 3); // Mask to 0..3 range

        // (1) Shift 2 words (128 bits)
        // If (word_sh & 2) != 0, move [w0, w1, w2, w3] -> [0, 0, w0, w1]
        let move_2 = builder.ins().band_imm(word_sh_masked, 2);
        let cond_2 = builder.ins().icmp_imm(IntCC::NotEqual, move_2, 0);

        let t0 = builder.ins().select(cond_2, zero, val[0]);
        let t1 = builder.ins().select(cond_2, zero, val[1]);
        let t2 = builder.ins().select(cond_2, val[0], val[2]);
        let t3 = builder.ins().select(cond_2, val[1], val[3]);

        // (2) Shift 1 word (64 bits)
        // If (word_sh & 1) != 0, move [t0, t1, t2, t3] -> [0, t0, t1, t2]
        let move_1 = builder.ins().band_imm(word_sh_masked, 1);
        let cond_1 = builder.ins().icmp_imm(IntCC::NotEqual, move_1, 0);

        let r0 = builder.ins().select(cond_1, zero, t0);
        let r1 = builder.ins().select(cond_1, t0, t1);
        let r2 = builder.ins().select(cond_1, t1, t2);
        let r3 = builder.ins().select(cond_1, t2, t3);

        // 3. Bit Shift (Fine Shift by 0..63 bits)
        // bit_sh = amount % 64
        let bit_sh = builder.ins().band_imm(amount, 63);

        // Calculate carry shift amount: inv_sh = (64 - bit_sh) % 64
        let c64 = builder.ins().iconst(types::I64, 64);
        let sub = builder.ins().isub(c64, bit_sh);
        let inv_sh = builder.ins().band_imm(sub, 63);

        // Check if bit_sh > 0. If bit_sh == 0, we must NOT apply the carry shift
        // because `x >> 64` is undefined behavior or wraps around in some IRs.
        let has_bits = builder.ins().icmp_imm(IntCC::NotEqual, bit_sh, 0);

        // w0: only left shift
        let w0_sh = builder.ins().ishl(r0, bit_sh);

        // w1: (r1 << bit_sh) | (r0 >> inv_sh)
        let w1_main = builder.ins().ishl(r1, bit_sh);
        let w1_carry = builder.ins().ushr(r0, inv_sh);
        // If bit_sh == 0, carry should be 0 (do not shift by 64)
        let w1_carry_safe = builder.ins().select(has_bits, w1_carry, zero);
        let w1_sh = builder.ins().bor(w1_main, w1_carry_safe);

        // w2: (r2 << bit_sh) | (r1 >> inv_sh)
        let w2_main = builder.ins().ishl(r2, bit_sh);
        let w2_carry = builder.ins().ushr(r1, inv_sh);
        let w2_carry_safe = builder.ins().select(has_bits, w2_carry, zero);
        let w2_sh = builder.ins().bor(w2_main, w2_carry_safe);

        // w3: (r3 << bit_sh) | (r2 >> inv_sh)
        let w3_main = builder.ins().ishl(r3, bit_sh);
        let w3_carry = builder.ins().ushr(r2, inv_sh);
        let w3_carry_safe = builder.ins().select(has_bits, w3_carry, zero);
        let w3_sh = builder.ins().bor(w3_main, w3_carry_safe);

        // 4. Final Select based on Oversized Check
        // If oversized, return 0. Else, return calculated value.
        let f0 = builder.ins().select(is_oversized, zero, w0_sh);
        let f1 = builder.ins().select(is_oversized, zero, w1_sh);
        let f2 = builder.ins().select(is_oversized, zero, w2_sh);
        let f3 = builder.ins().select(is_oversized, zero, w3_sh);

        [f0, f1, f2, f3]
    }

    // SHR (Logical Shift Right)
    pub fn u256_shr(
        builder: &mut FunctionBuilder,
        shift: [Value; 4],
        val: [Value; 4],
    ) -> [Value; 4] {
        let zero = builder.ins().iconst(types::I64, 0);

        // 1. Check for Oversized Shift (Shift >= 256)
        let high_words = builder.ins().bor(shift[1], shift[2]);
        let high_nonzero = builder.ins().bor(high_words, shift[3]);
        let low_large = builder
            .ins()
            .icmp_imm(IntCC::UnsignedGreaterThanOrEqual, shift[0], 256);
        let any_high = builder.ins().icmp_imm(IntCC::NotEqual, high_nonzero, 0);
        let is_oversized = builder.ins().bor(low_large, any_high);

        let amount = shift[0];
        let word_sh = builder.ins().ushr_imm(amount, 6);
        let word_sh_masked = builder.ins().band_imm(word_sh, 3);

        // 2. Word Shuffle (Right Direction)

        // (1) Shift 2 words (128 bits)
        // [w0, w1, w2, w3] -> [w2, w3, 0, 0]
        let move_2 = builder.ins().band_imm(word_sh_masked, 2);
        let cond_2 = builder.ins().icmp_imm(IntCC::NotEqual, move_2, 0);

        let t0 = builder.ins().select(cond_2, val[2], val[0]);
        let t1 = builder.ins().select(cond_2, val[3], val[1]);
        let t2 = builder.ins().select(cond_2, zero, val[2]);
        let t3 = builder.ins().select(cond_2, zero, val[3]);

        // (2) Shift 1 word (64 bits)
        // [t0, t1, t2, t3] -> [t1, t2, t3, 0]
        let move_1 = builder.ins().band_imm(word_sh_masked, 1);
        let cond_1 = builder.ins().icmp_imm(IntCC::NotEqual, move_1, 0);

        let r0 = builder.ins().select(cond_1, t1, t0);
        let r1 = builder.ins().select(cond_1, t2, t1);
        let r2 = builder.ins().select(cond_1, t3, t2);
        let r3 = builder.ins().select(cond_1, zero, t3);

        // 3. Bit Shift (Right Direction)
        let bit_sh = builder.ins().band_imm(amount, 63);
        let c64 = builder.ins().iconst(types::I64, 64);
        let sub = builder.ins().isub(c64, bit_sh);
        let inv_sh = builder.ins().band_imm(sub, 63);
        let has_bits = builder.ins().icmp_imm(IntCC::NotEqual, bit_sh, 0);

        // w3: only right shift
        let w3_sh = builder.ins().ushr(r3, bit_sh);

        // w2: (r2 >> bit_sh) | (r3 << inv_sh)
        // Carry comes from the upper word (left)
        let w2_main = builder.ins().ushr(r2, bit_sh);
        let w2_carry = builder.ins().ishl(r3, inv_sh);
        let w2_carry_safe = builder.ins().select(has_bits, w2_carry, zero);
        let w2_sh = builder.ins().bor(w2_main, w2_carry_safe);

        // w1: (r1 >> bit_sh) | (r2 << inv_sh)
        let w1_main = builder.ins().ushr(r1, bit_sh);
        let w1_carry = builder.ins().ishl(r2, inv_sh);
        let w1_carry_safe = builder.ins().select(has_bits, w1_carry, zero);
        let w1_sh = builder.ins().bor(w1_main, w1_carry_safe);

        // w0: (r0 >> bit_sh) | (r1 << inv_sh)
        let w0_main = builder.ins().ushr(r0, bit_sh);
        let w0_carry = builder.ins().ishl(r1, inv_sh);
        let w0_carry_safe = builder.ins().select(has_bits, w0_carry, zero);
        let w0_sh = builder.ins().bor(w0_main, w0_carry_safe);

        // 4. Final Select
        let f0 = builder.ins().select(is_oversized, zero, w0_sh);
        let f1 = builder.ins().select(is_oversized, zero, w1_sh);
        let f2 = builder.ins().select(is_oversized, zero, w2_sh);
        let f3 = builder.ins().select(is_oversized, zero, w3_sh);

        [f0, f1, f2, f3]
    }

    // SAR (Arithmetic Shift Right)
    pub fn u256_sar(
        builder: &mut FunctionBuilder,
        shift: [Value; 4],
        val: [Value; 4],
    ) -> [Value; 4] {
        // Extract Sign Bit: The most significant bit of the highest word (w3)
        let w3 = val[3];
        // sshr_imm with 63 will propagate the sign bit across the entire i64
        // Result: 0 if positive, -1 (all 1s) if negative
        let sign_bit = builder.ins().sshr_imm(w3, 63);

        // 1. Check for Oversized Shift
        let high_words = builder.ins().bor(shift[1], shift[2]);
        let high_nonzero = builder.ins().bor(high_words, shift[3]);
        let low_large = builder
            .ins()
            .icmp_imm(IntCC::UnsignedGreaterThanOrEqual, shift[0], 256);
        let any_high = builder.ins().icmp_imm(IntCC::NotEqual, high_nonzero, 0);
        let is_oversized = builder.ins().bor(low_large, any_high);

        // IMPORTANT: For SAR, an oversized shift results in the "fill" value.
        // If the number is negative, we fill with -1. If positive, we fill with 0.
        let fill = sign_bit;

        let amount = shift[0];
        let word_sh = builder.ins().ushr_imm(amount, 6);
        let word_sh_masked = builder.ins().band_imm(word_sh, 3);

        // 2. Word Shuffle (Right Direction with Sign Fill)

        // (1) Shift 2 words
        // [w0, w1, w2, w3] -> [w2, w3, fill, fill]
        let move_2 = builder.ins().band_imm(word_sh_masked, 2);
        let cond_2 = builder.ins().icmp_imm(IntCC::NotEqual, move_2, 0);

        let t0 = builder.ins().select(cond_2, val[2], val[0]);
        let t1 = builder.ins().select(cond_2, val[3], val[1]);
        let t2 = builder.ins().select(cond_2, fill, val[2]);
        let t3 = builder.ins().select(cond_2, fill, val[3]);

        // (2) Shift 1 word
        // [t0, t1, t2, t3] -> [t1, t2, t3, fill]
        let move_1 = builder.ins().band_imm(word_sh_masked, 1);
        let cond_1 = builder.ins().icmp_imm(IntCC::NotEqual, move_1, 0);

        let r0 = builder.ins().select(cond_1, t1, t0);
        let r1 = builder.ins().select(cond_1, t2, t1);
        let r2 = builder.ins().select(cond_1, t3, t2);
        let r3 = builder.ins().select(cond_1, fill, t3);

        // 3. Bit Shift (Arithmetic)
        let bit_sh = builder.ins().band_imm(amount, 63);
        let c64 = builder.ins().iconst(types::I64, 64);
        let sub = builder.ins().isub(c64, bit_sh);
        let inv_sh = builder.ins().band_imm(sub, 63);
        let has_bits = builder.ins().icmp_imm(IntCC::NotEqual, bit_sh, 0);

        // w3: Arithmetic Shift (SSHR) to preserve sign extension within the word
        let w3_sh = builder.ins().sshr(r3, bit_sh);

        // w2: (r2 >> bit_sh) | (r3 << inv_sh)
        // Lower words are treated as unsigned logic (USHR) because the sign is handled by w3 and 'fill'
        let w2_main = builder.ins().ushr(r2, bit_sh);
        let w2_carry = builder.ins().ishl(r3, inv_sh);
        let zero = builder.ins().iconst(types::I64, 0);
        // If bit_sh == 0, carry is 0
        let w2_carry_safe = builder.ins().select(has_bits, w2_carry, zero);
        let w2_sh = builder.ins().bor(w2_main, w2_carry_safe);

        // w1: (r1 >> bit_sh) | (r2 << inv_sh)
        let w1_main = builder.ins().ushr(r1, bit_sh);
        let w1_carry = builder.ins().ishl(r2, inv_sh);
        let w1_carry_safe = builder.ins().select(has_bits, w1_carry, zero);
        let w1_sh = builder.ins().bor(w1_main, w1_carry_safe);

        // w0: (r0 >> bit_sh) | (r1 << inv_sh)
        let w0_main = builder.ins().ushr(r0, bit_sh);
        let w0_carry = builder.ins().ishl(r1, inv_sh);
        let w0_carry_safe = builder.ins().select(has_bits, w0_carry, zero);
        let w0_sh = builder.ins().bor(w0_main, w0_carry_safe);

        // 4. Final Select
        // If oversized, return the fill value (0 or -1).
        let f0 = builder.ins().select(is_oversized, fill, w0_sh);
        let f1 = builder.ins().select(is_oversized, fill, w1_sh);
        let f2 = builder.ins().select(is_oversized, fill, w2_sh);
        let f3 = builder.ins().select(is_oversized, fill, w3_sh);

        [f0, f1, f2, f3]
    }
}
