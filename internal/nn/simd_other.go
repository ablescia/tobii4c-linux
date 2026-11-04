//go:build !amd64

package nn

var hasAVX2FMA = false

func axpy4x2AVX(d0, d1, s0, s1, s2, s3 *float32, n int, w *[8]float32) { panic("unreachable") }
func axpy4AVX(d, s0, s1, s2, s3 *float32, n int, w *[4]float32)        { panic("unreachable") }
func axpy1AVX(d, s *float32, n int, w float32)                         { panic("unreachable") }
