#include "textflag.h"

// func cpuid(eaxArg, ecxArg uint32) (a, b, c, d uint32)
TEXT ·cpuid(SB), NOSPLIT, $0-24
	MOVL eaxArg+0(FP), AX
	MOVL ecxArg+4(FP), CX
	CPUID
	MOVL AX, a+8(FP)
	MOVL BX, b+12(FP)
	MOVL CX, c+16(FP)
	MOVL DX, d+20(FP)
	RET

// func xgetbv0() (eax, edx uint32)
TEXT ·xgetbv0(SB), NOSPLIT, $0-8
	MOVL $0, CX
	XGETBV
	MOVL AX, eax+0(FP)
	MOVL DX, edx+4(FP)
	RET

// func axpy4x2AVX(d0, d1, s0, s1, s2, s3 *float32, n int, w *[8]float32)
//
// Per i in [0,n), n multiplo di 8:
//   d0[i] += w[0]*s0[i] + w[1]*s1[i] + w[2]*s2[i] + w[3]*s3[i]
//   d1[i] += w[4]*s0[i] + w[5]*s1[i] + w[6]*s2[i] + w[7]*s3[i]
TEXT ·axpy4x2AVX(SB), NOSPLIT, $0-64
	MOVQ d0+0(FP), DI
	MOVQ d1+8(FP), SI
	MOVQ s0+16(FP), R8
	MOVQ s1+24(FP), R9
	MOVQ s2+32(FP), R10
	MOVQ s3+40(FP), R11
	MOVQ n+48(FP), CX
	MOVQ w+56(FP), AX
	VBROADCASTSS 0(AX), Y0
	VBROADCASTSS 4(AX), Y1
	VBROADCASTSS 8(AX), Y2
	VBROADCASTSS 12(AX), Y3
	VBROADCASTSS 16(AX), Y4
	VBROADCASTSS 20(AX), Y5
	VBROADCASTSS 24(AX), Y6
	VBROADCASTSS 28(AX), Y7
	XORQ BX, BX
	SHRQ $3, CX
	JZ   done42
loop42:
	VMOVUPS (R8)(BX*1), Y8
	VMOVUPS (R9)(BX*1), Y9
	VMOVUPS (R10)(BX*1), Y10
	VMOVUPS (R11)(BX*1), Y11
	VMOVUPS (DI)(BX*1), Y12
	VMOVUPS (SI)(BX*1), Y13
	VFMADD231PS Y8, Y0, Y12
	VFMADD231PS Y8, Y4, Y13
	VFMADD231PS Y9, Y1, Y12
	VFMADD231PS Y9, Y5, Y13
	VFMADD231PS Y10, Y2, Y12
	VFMADD231PS Y10, Y6, Y13
	VFMADD231PS Y11, Y3, Y12
	VFMADD231PS Y11, Y7, Y13
	VMOVUPS Y12, (DI)(BX*1)
	VMOVUPS Y13, (SI)(BX*1)
	ADDQ $32, BX
	DECQ CX
	JNZ  loop42
done42:
	VZEROUPPER
	RET

// func axpy4AVX(d, s0, s1, s2, s3 *float32, n int, w *[4]float32)
//
// Per i in [0,n), n multiplo di 8:
//   d[i] += w[0]*s0[i] + w[1]*s1[i] + w[2]*s2[i] + w[3]*s3[i]
TEXT ·axpy4AVX(SB), NOSPLIT, $0-56
	MOVQ d+0(FP), DI
	MOVQ s0+8(FP), R8
	MOVQ s1+16(FP), R9
	MOVQ s2+24(FP), R10
	MOVQ s3+32(FP), R11
	MOVQ n+40(FP), CX
	MOVQ w+48(FP), AX
	VBROADCASTSS 0(AX), Y0
	VBROADCASTSS 4(AX), Y1
	VBROADCASTSS 8(AX), Y2
	VBROADCASTSS 12(AX), Y3
	XORQ BX, BX
	SHRQ $3, CX
	JZ   done4
loop4:
	VMOVUPS (DI)(BX*1), Y12
	VFMADD231PS (R8)(BX*1), Y0, Y12
	VFMADD231PS (R9)(BX*1), Y1, Y12
	VFMADD231PS (R10)(BX*1), Y2, Y12
	VFMADD231PS (R11)(BX*1), Y3, Y12
	VMOVUPS Y12, (DI)(BX*1)
	ADDQ $32, BX
	DECQ CX
	JNZ  loop4
done4:
	VZEROUPPER
	RET

// func axpy1AVX(d, s *float32, n int, w float32)
//
// Per i in [0,n), n multiplo di 8: d[i] += w*s[i]
TEXT ·axpy1AVX(SB), NOSPLIT, $0-28
	MOVQ d+0(FP), DI
	MOVQ s+8(FP), R8
	MOVQ n+16(FP), CX
	VBROADCASTSS w+24(FP), Y0
	XORQ BX, BX
	SHRQ $3, CX
	JZ   done1
loop1:
	VMOVUPS (DI)(BX*1), Y12
	VFMADD231PS (R8)(BX*1), Y0, Y12
	VMOVUPS Y12, (DI)(BX*1)
	ADDQ $32, BX
	DECQ CX
	JNZ  loop1
done1:
	VZEROUPPER
	RET
