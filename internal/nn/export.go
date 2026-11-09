package nn

// Kernel esportati per gli altri interpreti del progetto (internal/tflite):
// stessi loop AVX2/FMA delle convoluzioni di questo pacchetto.

// Axpy calcola dst[i] += w·src[i]; src deve avere almeno len(dst) elementi.
func Axpy(dst, src []float32, w float32) { axpy(dst, src, w) }

// Axpy4 calcola dst[i] += w[0]·s0[i] + w[1]·s1[i] + w[2]·s2[i] + w[3]·s3[i];
// le sorgenti devono avere almeno len(dst) elementi.
func Axpy4(dst, s0, s1, s2, s3 []float32, w *[4]float32) {
	n := len(dst)
	s0, s1, s2, s3 = s0[:n], s1[:n], s2[:n], s3[:n]
	i0 := 0
	if n8 := n &^ 7; hasAVX2FMA && n8 > 0 {
		axpy4AVX(&dst[0], &s0[0], &s1[0], &s2[0], &s3[0], n8, w)
		i0 = n8
	}
	for i := i0; i < n; i++ {
		dst[i] += w[0]*s0[i] + w[1]*s1[i] + w[2]*s2[i] + w[3]*s3[i]
	}
}

// HasAVX2 dice se i kernel usano AVX2/FMA.
func HasAVX2() bool { return hasAVX2FMA }
