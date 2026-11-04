package nn

// Kernel AVX2/FMA (simd_amd64.s). Usati solo se la CPU e il sistema
// operativo li supportano; altrimenti si resta sui loop in Go.

func cpuid(eaxArg, ecxArg uint32) (a, b, c, d uint32)
func xgetbv0() (eax, edx uint32)

//go:noescape
func axpy4x2AVX(d0, d1, s0, s1, s2, s3 *float32, n int, w *[8]float32)

//go:noescape
func axpy4AVX(d, s0, s1, s2, s3 *float32, n int, w *[4]float32)

//go:noescape
func axpy1AVX(d, s *float32, n int, w float32)

var hasAVX2FMA = detectAVX2FMA()

func detectAVX2FMA() bool {
	maxID, _, _, _ := cpuid(0, 0)
	if maxID < 7 {
		return false
	}
	_, _, c1, _ := cpuid(1, 0)
	const fma, osxsave, avx = 1 << 12, 1 << 27, 1 << 28
	if c1&fma == 0 || c1&osxsave == 0 || c1&avx == 0 {
		return false
	}
	if xcr0, _ := xgetbv0(); xcr0&6 != 6 { // stato XMM e YMM salvato dal SO
		return false
	}
	_, b7, _, _ := cpuid(7, 0)
	return b7&(1<<5) != 0 // AVX2
}
