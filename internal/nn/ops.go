package nn

import (
	"errors"
	"fmt"
	"math"
)

func attrInts(n nodeProto, name string, def []int) []int {
	a, ok := n.attrs[name]
	if !ok || len(a.ints) == 0 {
		return def
	}
	out := make([]int, len(a.ints))
	for i, v := range a.ints {
		out[i] = int(v)
	}
	return out
}

func attrInt(n nodeProto, name string, def int) int {
	if a, ok := n.attrs[name]; ok {
		return int(a.i)
	}
	return def
}

// rejectAttrs fallisce se il nodo usa attributi con valori non supportati.
func rejectAttrs(n nodeProto, allowed ...string) error {
	for name := range n.attrs {
		ok := false
		for _, a := range allowed {
			if a == name {
				ok = true
			}
		}
		if !ok {
			return fmt.Errorf("unsupported attribute %q", name)
		}
	}
	if a, ok := n.attrs["auto_pad"]; ok && a.s != "" && a.s != "NOTSET" {
		return fmt.Errorf("unsupported auto_pad %q", a.s)
	}
	return nil
}

func allOnes(v []int) bool {
	for _, x := range v {
		if x != 1 {
			return false
		}
	}
	return true
}

func (st *step) data(i int) (*value, error) {
	if i >= len(st.in) || st.in[i] == nil {
		return nil, fmt.Errorf("missing input %d", i)
	}
	v := st.in[i]
	if v.konst && v.data == nil {
		return nil, fmt.Errorf("input %q is not float32", v.name)
	}
	return v, nil
}

func (st *step) constInts(i int) ([]int64, error) {
	if i >= len(st.in) || st.in[i] == nil || !st.in[i].konst || st.in[i].i64 == nil {
		return nil, fmt.Errorf("input %d must be a constant int64 tensor", i)
	}
	return st.in[i].i64, nil
}

// compile valida il nodo e restituisce la forma dell'uscita.
func (st *step) compile(n nodeProto) ([]int, error) {
	switch n.op {
	case "Conv":
		if err := rejectAttrs(n, "dilations", "group", "kernel_shape", "pads", "strides", "auto_pad"); err != nil {
			return nil, err
		}
		x, err := st.data(0)
		if err != nil {
			return nil, err
		}
		w, err := st.data(1)
		if err != nil {
			return nil, err
		}
		if len(x.shape) != 4 || len(w.shape) != 4 || !w.konst {
			return nil, errors.New("want 4-D input and constant 4-D weights")
		}
		if len(st.in) > 2 && st.in[2] != nil {
			b, err := st.data(2)
			if err != nil {
				return nil, err
			}
			if !b.konst || size(b.shape) != w.shape[0] {
				return nil, errors.New("bias must be constant with one value per output channel")
			}
		}
		if !allOnes(attrInts(n, "dilations", []int{1, 1})) {
			return nil, errors.New("dilations != 1")
		}
		ks := attrInts(n, "kernel_shape", w.shape[2:])
		s := attrInts(n, "strides", []int{1, 1})
		p := attrInts(n, "pads", []int{0, 0, 0, 0})
		if len(ks) != 2 || len(s) != 2 || len(p) != 4 || ks[0] != w.shape[2] || ks[1] != w.shape[3] {
			return nil, errors.New("bad kernel_shape, strides or pads")
		}
		st.group = attrInt(n, "group", 1)
		c, m := x.shape[1], w.shape[0]
		st.kh, st.kw, st.sh, st.sw = ks[0], ks[1], s[0], s[1]
		st.pt, st.pl, st.pb, st.pr = p[0], p[1], p[2], p[3]
		switch {
		case st.group == 1 && w.shape[1] == c:
			st.pointwise = st.kh == 1 && st.kw == 1 && st.sh == 1 && st.sw == 1 && p[0]|p[1]|p[2]|p[3] == 0
		case st.group == c && m == c && w.shape[1] == 1:
			st.depthwise = true
		default:
			return nil, fmt.Errorf("unsupported group %d (C=%d, M=%d)", st.group, c, m)
		}
		if st.sh < 1 || st.sw < 1 {
			return nil, errors.New("bad strides")
		}
		ho := (x.shape[2]+st.pt+st.pb-st.kh)/st.sh + 1
		wo := (x.shape[3]+st.pl+st.pr-st.kw)/st.sw + 1
		if ho <= 0 || wo <= 0 {
			return nil, errors.New("empty output")
		}
		return []int{1, m, ho, wo}, nil

	case "PRelu":
		x, err := st.data(0)
		if err != nil {
			return nil, err
		}
		sl, err := st.data(1)
		if err != nil {
			return nil, err
		}
		switch ns := size(sl.shape); {
		case ns == 1:
		case len(x.shape) == 4 && ns == x.shape[1]:
			st.slopePerChannel = true
		default:
			return nil, fmt.Errorf("slope shape %v for input %v", sl.shape, x.shape)
		}
		return x.shape, nil

	case "Add":
		a, err := st.data(0)
		if err != nil {
			return nil, err
		}
		b, err := st.data(1)
		if err != nil {
			return nil, err
		}
		if fmt.Sprint(a.shape) != fmt.Sprint(b.shape) {
			return nil, fmt.Errorf("broadcasting not supported: %v + %v", a.shape, b.shape)
		}
		return a.shape, nil

	case "MaxPool":
		if err := rejectAttrs(n, "kernel_shape", "strides", "pads", "auto_pad", "ceil_mode", "dilations", "storage_order"); err != nil {
			return nil, err
		}
		x, err := st.data(0)
		if err != nil {
			return nil, err
		}
		ks := attrInts(n, "kernel_shape", nil)
		s := attrInts(n, "strides", []int{1, 1})
		p := attrInts(n, "pads", []int{0, 0, 0, 0})
		if len(x.shape) != 4 || len(ks) != 2 || len(s) != 2 || p[0]|p[1]|p[2]|p[3] != 0 ||
			attrInt(n, "ceil_mode", 0) != 0 || !allOnes(attrInts(n, "dilations", []int{1, 1})) {
			return nil, errors.New("only 2-D max pooling without padding")
		}
		st.kh, st.kw, st.sh, st.sw = ks[0], ks[1], s[0], s[1]
		ho := (x.shape[2]-st.kh)/st.sh + 1
		wo := (x.shape[3]-st.kw)/st.sw + 1
		if ho <= 0 || wo <= 0 {
			return nil, errors.New("empty output")
		}
		return []int{1, x.shape[1], ho, wo}, nil

	case "Pad":
		if err := rejectAttrs(n, "mode"); err != nil {
			return nil, err
		}
		if a, ok := n.attrs["mode"]; ok && a.s != "constant" {
			return nil, fmt.Errorf("unsupported mode %q", a.s)
		}
		x, err := st.data(0)
		if err != nil {
			return nil, err
		}
		pads, err := st.constInts(1)
		if err != nil {
			return nil, err
		}
		if len(st.in) > 2 && st.in[2] != nil {
			cv := st.in[2]
			if !cv.konst || len(cv.data) != 1 || cv.data[0] != 0 {
				return nil, errors.New("only zero padding")
			}
		}
		if len(st.in) > 3 && st.in[3] != nil {
			return nil, errors.New("axes input not supported")
		}
		r := len(x.shape)
		if len(pads) != 2*r || r != 4 {
			return nil, fmt.Errorf("pads %v for rank %d", pads, r)
		}
		out := make([]int, r)
		st.pads = make([]int, 2*r)
		for i := range pads {
			if pads[i] < 0 || pads[i] > 1<<16 {
				return nil, errors.New("negative or huge pads")
			}
			st.pads[i] = int(pads[i])
		}
		for i := 0; i < r; i++ {
			out[i] = x.shape[i] + st.pads[i] + st.pads[r+i]
		}
		return out, nil

	case "Reshape":
		if err := rejectAttrs(n, "allowzero"); err != nil {
			return nil, err
		}
		if attrInt(n, "allowzero", 0) != 0 {
			return nil, errors.New("allowzero not supported")
		}
		x, err := st.data(0)
		if err != nil {
			return nil, err
		}
		target, err := st.constInts(1)
		if err != nil {
			return nil, err
		}
		out := make([]int, len(target))
		known, infer := 1, -1
		for i, d := range target {
			switch {
			case d == 0 && i < len(x.shape):
				out[i] = x.shape[i]
			case d == -1 && infer < 0:
				infer = i
				continue
			case d > 0:
				out[i] = int(d)
			default:
				return nil, fmt.Errorf("bad target shape %v", target)
			}
			known *= out[i]
		}
		if infer >= 0 {
			if known == 0 || size(x.shape)%known != 0 {
				return nil, fmt.Errorf("cannot reshape %v to %v", x.shape, target)
			}
			out[infer] = size(x.shape) / known
		}
		if size(out) != size(x.shape) {
			return nil, fmt.Errorf("cannot reshape %v to %v", x.shape, target)
		}
		return out, nil

	case "Transpose":
		x, err := st.data(0)
		if err != nil {
			return nil, err
		}
		perm := attrInts(n, "perm", nil)
		if len(perm) != len(x.shape) || len(perm) != 4 {
			return nil, errors.New("only 4-D transposes with explicit perm")
		}
		seen := [4]bool{}
		out := make([]int, 4)
		for i, p := range perm {
			if p < 0 || p > 3 || seen[p] {
				return nil, fmt.Errorf("bad perm %v", perm)
			}
			seen[p] = true
			out[i] = x.shape[p]
		}
		st.perm = perm
		return out, nil

	case "Sigmoid":
		x, err := st.data(0)
		if err != nil {
			return nil, err
		}
		return x.shape, nil
	}
	return nil, fmt.Errorf("unsupported operator %q", n.op)
}

// cost stima le moltiplicazioni-accumulo del passo.
func (st *step) cost() int64 {
	if st.op != "Conv" {
		return 0
	}
	o := st.out.shape
	cin := st.in[1].shape[1]
	return int64(size(o)) * int64(cin*st.kh*st.kw)
}

func (m *Model) exec(st *step) {
	out := st.out.data
	switch st.op {
	case "Conv":
		switch {
		case st.pointwise:
			m.convPointwise(st)
		case st.depthwise:
			m.convDepthwise(st)
		default:
			m.convDirect(st)
		}
	case "PRelu":
		x, sl := st.in[0].data, st.in[1].data
		if !st.slopePerChannel {
			a := sl[0]
			for i, v := range x {
				if v < 0 {
					v *= a
				}
				out[i] = v
			}
			return
		}
		c := st.in[0].shape[1]
		hw := len(x) / c
		for ch := 0; ch < c; ch++ {
			a := sl[ch]
			xs, os := x[ch*hw:(ch+1)*hw], out[ch*hw:(ch+1)*hw]
			for i, v := range xs {
				if v < 0 {
					v *= a
				}
				os[i] = v
			}
		}
	case "Add":
		a, b := st.in[0].data, st.in[1].data
		c, hw := 1, len(a)
		if len(st.out.shape) == 4 {
			c, hw = st.out.shape[1], st.out.shape[2]*st.out.shape[3]
		}
		fn := st.fns[0]
		if fn == nil {
			fn = func(lo, hi int) {
				as, bs, os := a[lo*hw:hi*hw], b[lo*hw:hi*hw], out[lo*hw:hi*hw]
				bs, os = bs[:len(as)], os[:len(as)]
				for i := range as {
					os[i] = as[i] + bs[i]
				}
				applyPRelu(st.prelu, out, hw, lo, hi)
			}
			st.fns[0] = fn
		}
		m.par(c, fn)
	case "MaxPool":
		x := st.in[0].data
		c, h, w := st.in[0].shape[1], st.in[0].shape[2], st.in[0].shape[3]
		ho, wo := st.out.shape[2], st.out.shape[3]
		if st.kh == 2 && st.kw == 2 && st.sh == 2 && st.sw == 2 {
			fn := st.fns[0]
			if fn == nil {
				fn = func(lo, hi int) {
					for ch := lo; ch < hi; ch++ {
						for oy := 0; oy < ho; oy++ {
							r0 := x[(ch*h+2*oy)*w:][:w]
							r1 := x[(ch*h+2*oy+1)*w:][:w]
							orow := out[(ch*ho+oy)*wo:][:wo]
							for ox := range orow {
								orow[ox] = max(r0[2*ox], r0[2*ox+1], r1[2*ox], r1[2*ox+1])
							}
						}
					}
				}
				st.fns[0] = fn
			}
			m.par(c, fn)
			return
		}
		for ch := 0; ch < c; ch++ {
			xs := x[ch*h*w:]
			os := out[ch*ho*wo:]
			for oy := 0; oy < ho; oy++ {
				for ox := 0; ox < wo; ox++ {
					best := float32(math.Inf(-1))
					for ky := 0; ky < st.kh; ky++ {
						row := xs[(oy*st.sh+ky)*w+ox*st.sw:]
						for kx := 0; kx < st.kw; kx++ {
							if v := row[kx]; v > best {
								best = v
							}
						}
					}
					os[oy*wo+ox] = best
				}
			}
		}
	case "Pad":
		pad4(st.in[0].data, st.in[0].shape, out, st.out.shape, st.pads)
	case "Reshape":
		copy(out, st.in[0].data)
	case "Transpose":
		transpose4(st.in[0].data, st.in[0].shape, out, st.perm)
	case "Sigmoid":
		for i, v := range st.in[0].data {
			out[i] = float32(1 / (1 + math.Exp(-float64(v))))
		}
	}
}

func pad4(x []float32, xs []int, out []float32, os []int, pads []int) {
	clear(out)
	for n := 0; n < xs[0]; n++ {
		for c := 0; c < xs[1]; c++ {
			for h := 0; h < xs[2]; h++ {
				src := x[((n*xs[1]+c)*xs[2]+h)*xs[3]:][:xs[3]]
				on, oc, oh := n+pads[0], c+pads[1], h+pads[2]
				dst := out[((on*os[1]+oc)*os[2]+oh)*os[3]+pads[3]:]
				copy(dst, src)
			}
		}
	}
}

func transpose4(x []float32, xs []int, out []float32, perm []int) {
	// strides dell'ingresso
	st := [4]int{xs[1] * xs[2] * xs[3], xs[2] * xs[3], xs[3], 1}
	var os [4]int
	for i := range os {
		os[i] = xs[perm[i]]
	}
	s0, s1, s2, s3 := st[perm[0]], st[perm[1]], st[perm[2]], st[perm[3]]
	k := 0
	for a := 0; a < os[0]; a++ {
		for b := 0; b < os[1]; b++ {
			for c := 0; c < os[2]; c++ {
				base := a*s0 + b*s1 + c*s2
				for d := 0; d < os[3]; d++ {
					out[k] = x[base+d*s3]
					k++
				}
			}
		}
	}
}

func bias(st *step, ch int) float32 {
	if len(st.in) > 2 && st.in[2] != nil {
		return st.in[2].data[ch]
	}
	return 0
}

// applyPRelu applica la PRelu fusa ai canali [c0,c1) di out.
func applyPRelu(prelu, out []float32, hw, c0, c1 int) {
	if prelu == nil {
		return
	}
	for ch := c0; ch < c1; ch++ {
		a := prelu[0]
		if len(prelu) > 1 {
			a = prelu[ch]
		}
		seg := out[ch*hw : (ch+1)*hw]
		for i, v := range seg {
			if v < 0 {
				seg[i] = v * a
			}
		}
	}
}

// gemm calcola out[o][p] = bias[o] + Σ_k w[o][k]·x[k][p] (+ PRelu fusa) per
// le uscite [o0,o1) e i pixel [p0,p1). x ha K righe di P valori. Lavora su
// due uscite e quattro righe alla volta: ogni valore d'ingresso caricato
// serve due moltiplicazioni-accumulo.
func gemm(st *step, w, x, out []float32, K, P, o0, o1, p0, p1 int) {
	o := o0
	for ; o+2 <= o1; o += 2 {
		d0 := out[o*P+p0 : o*P+p1]
		d1 := out[(o+1)*P+p0 : (o+1)*P+p1]
		d1 = d1[:len(d0)]
		b0, b1 := bias(st, o), bias(st, o+1)
		for i := range d0 {
			d0[i], d1[i] = b0, b1
		}
		w0r, w1r := w[o*K:(o+1)*K], w[(o+1)*K:(o+2)*K]
		k := 0
		for ; k+4 <= K; k += 4 {
			a0, a1, a2, a3 := w0r[k], w0r[k+1], w0r[k+2], w0r[k+3]
			c0, c1, c2, c3 := w1r[k], w1r[k+1], w1r[k+2], w1r[k+3]
			s0 := x[k*P+p0 : k*P+p1]
			s1 := x[(k+1)*P+p0 : (k+1)*P+p1]
			s2 := x[(k+2)*P+p0 : (k+2)*P+p1]
			s3 := x[(k+3)*P+p0 : (k+3)*P+p1]
			s0, s1, s2, s3 = s0[:len(d0)], s1[:len(d0)], s2[:len(d0)], s3[:len(d0)]
			i0 := 0
			if n8 := len(d0) &^ 7; hasAVX2FMA && n8 > 0 {
				w8 := [8]float32{a0, a1, a2, a3, c0, c1, c2, c3}
				axpy4x2AVX(&d0[0], &d1[0], &s0[0], &s1[0], &s2[0], &s3[0], n8, &w8)
				i0 = n8
			}
			for i := i0; i < len(d0); i++ {
				v0, v1, v2, v3 := s0[i], s1[i], s2[i], s3[i]
				d0[i] += a0*v0 + a1*v1 + a2*v2 + a3*v3
				d1[i] += c0*v0 + c1*v1 + c2*v2 + c3*v3
			}
		}
		for ; k < K; k++ {
			s0 := x[k*P+p0 : k*P+p1]
			axpy(d0, s0, w0r[k])
			axpy(d1, s0, w1r[k])
		}
		preluRow(st, d0, o)
		preluRow(st, d1, o+1)
	}
	for ; o < o1; o++ {
		dst := out[o*P+p0 : o*P+p1]
		bv := bias(st, o)
		for i := range dst {
			dst[i] = bv
		}
		wr := w[o*K : (o+1)*K]
		k := 0
		for ; k+4 <= K; k += 4 {
			s0 := x[k*P+p0 : k*P+p1]
			s1 := x[(k+1)*P+p0 : (k+1)*P+p1]
			s2 := x[(k+2)*P+p0 : (k+2)*P+p1]
			s3 := x[(k+3)*P+p0 : (k+3)*P+p1]
			s0, s1, s2, s3 = s0[:len(dst)], s1[:len(dst)], s2[:len(dst)], s3[:len(dst)]
			a0, a1, a2, a3 := wr[k], wr[k+1], wr[k+2], wr[k+3]
			i0 := 0
			if n8 := len(dst) &^ 7; hasAVX2FMA && n8 > 0 {
				w4 := [4]float32{a0, a1, a2, a3}
				axpy4AVX(&dst[0], &s0[0], &s1[0], &s2[0], &s3[0], n8, &w4)
				i0 = n8
			}
			for i := i0; i < len(dst); i++ {
				dst[i] += a0*s0[i] + a1*s1[i] + a2*s2[i] + a3*s3[i]
			}
		}
		for ; k < K; k++ {
			axpy(dst, x[k*P+p0:k*P+p1], wr[k])
		}
		preluRow(st, dst, o)
	}
}

// axpy: dst[i] += w·src[i].
func axpy(dst, src []float32, w float32) {
	src = src[:len(dst)]
	i0 := 0
	if n8 := len(dst) &^ 7; hasAVX2FMA && n8 > 0 {
		axpy1AVX(&dst[0], &src[0], n8, w)
		i0 = n8
	}
	for i := i0; i < len(dst); i++ {
		dst[i] += w * src[i]
	}
}

// preluRow applica la PRelu fusa a una riga del canale ch.
func preluRow(st *step, dst []float32, ch int) {
	if st.prelu == nil {
		return
	}
	a := st.prelu[0]
	if len(st.prelu) > 1 {
		a = st.prelu[ch]
	}
	for i, v := range dst {
		if v < 0 {
			dst[i] = v * a
		}
	}
}

// matmul divide il prodotto tra le goroutine: per pixel quando l'immagine è
// grande (i blocchi d'ingresso restano in cache), altrimenti per uscita.
func (m *Model) matmul(st *step, w, x []float32, K, P int) {
	out := st.out.data
	M := st.out.shape[1]
	if P == 1 {
		x = x[:K]
		fn := st.fns[0]
		if fn == nil {
			fn = func(lo, hi int) {
				for o := lo; o < hi; o++ {
					wr := w[o*K : (o+1)*K]
					var a0, a1, a2, a3 float32
					k := 0
					for ; k+4 <= K; k += 4 {
						a0 += wr[k] * x[k]
						a1 += wr[k+1] * x[k+1]
						a2 += wr[k+2] * x[k+2]
						a3 += wr[k+3] * x[k+3]
					}
					for ; k < K; k++ {
						a0 += wr[k] * x[k]
					}
					out[o] = bias(st, o) + (a0 + a1) + (a2 + a3)
					preluRow(st, out[o:o+1], o)
				}
			}
			st.fns[0] = fn
		}
		m.par(M, fn)
		return
	}
	const block = 256
	if P >= 2048 {
		nb := (P + block - 1) / block
		fn := st.fns[0]
		if fn == nil {
			fn = func(lo, hi int) {
				gemm(st, w, x, out, K, P, 0, M, lo*block, min(hi*block, P))
			}
			st.fns[0] = fn
		}
		m.par(nb, fn)
		return
	}
	fn := st.fns[0]
	if fn == nil {
		fn = func(lo, hi int) { gemm(st, w, x, out, K, P, lo, hi, 0, P) }
		st.fns[0] = fn
	}
	m.par(M, fn)
}

// convPointwise: convoluzione 1×1, la parte più costosa delle reti mobili.
func (m *Model) convPointwise(st *step) {
	hw := st.out.shape[2] * st.out.shape[3]
	m.matmul(st, st.in[1].data, st.in[0].data, st.in[0].shape[1], hw)
}

// convDirect: convoluzione generica (group 1) come im2col + prodotto.
func (m *Model) convDirect(st *step) {
	x := st.in[0].data
	c, h, wi := st.in[0].shape[1], st.in[0].shape[2], st.in[0].shape[3]
	ho, wo := st.out.shape[2], st.out.shape[3]
	kh, kw := st.kh, st.kw
	P := ho * wo
	K := c * kh * kw
	if st.col == nil {
		st.col = make([]float32, K*P)
	}
	col := st.col
	fn := st.fns[1]
	if fn == nil {
		fn = func(lo, hi int) {
			for k := lo; k < hi; k++ {
				ci, ky, kx := k/(kh*kw), (k/kw)%kh, k%kw
				row := col[k*P : (k+1)*P]
				xs := x[ci*h*wi : (ci+1)*h*wi]
				for oy := 0; oy < ho; oy++ {
					iy := oy*st.sh - st.pt + ky
					dst := row[oy*wo : (oy+1)*wo]
					if iy < 0 || iy >= h {
						clear(dst)
						continue
					}
					src := xs[iy*wi : (iy+1)*wi]
					for ox := range dst {
						ix := ox*st.sw - st.pl + kx
						if ix < 0 || ix >= wi {
							dst[ox] = 0
						} else {
							dst[ox] = src[ix]
						}
					}
				}
			}
		}
		st.fns[1] = fn
	}
	m.par(K, fn)
	m.matmul(st, st.in[1].data, col, K, P)
}

// convDepthwise: un filtro kh×kw per canale. A passo 1 ogni tap del kernel
// è una somma su una riga traslata (loop contigui, senza controlli interni);
// altrimenti si usa il caso generale.
func (m *Model) convDepthwise(st *step) {
	x, w, out := st.in[0].data, st.in[1].data, st.out.data
	c, h, wi := st.in[0].shape[1], st.in[0].shape[2], st.in[0].shape[3]
	ho, wo := st.out.shape[2], st.out.shape[3]
	kh, kw, sh, sw, pt, pl := st.kh, st.kw, st.sh, st.sw, st.pt, st.pl
	plane := sh == 1 && sw == 1 && ho == h && wo == wi
	fn := st.fns[0]
	if fn == nil {
		fn = func(lo, hi int) {
			for ch := lo; ch < hi; ch++ {
				xs := x[ch*h*wi : (ch+1)*h*wi]
				ws := w[ch*kh*kw : (ch+1)*kh*kw]
				os := out[ch*ho*wo : (ch+1)*ho*wo]
				bv := bias(st, ch)
				for i := range os {
					os[i] = bv
				}
				if plane {
					depthwisePlane(os, xs, ws, h, wi, kh, kw, pt, pl)
					applyPRelu(st.prelu, out, ho*wo, ch, ch+1)
					continue
				}
				for oy := 0; oy < ho; oy++ {
					orow := os[oy*wo : (oy+1)*wo]
					for ky := 0; ky < kh; ky++ {
						iy := oy*sh - pt + ky
						if iy < 0 || iy >= h {
							continue
						}
						irow := xs[iy*wi : (iy+1)*wi]
						wr := ws[ky*kw : (ky+1)*kw]
						if sw == 1 {
							for kx, wv := range wr {
								shift := kx - pl // ix = ox + shift
								a := max(0, -shift)
								b := min(wo, wi-shift)
								if b <= a {
									continue
								}
								axpy(orow[a:b], irow[a+shift:b+shift], wv)
							}
							continue
						}
						for ox := range orow {
							base := ox*sw - pl
							acc := orow[ox]
							for kx, wv := range wr {
								if ix := base + kx; ix >= 0 && ix < wi {
									acc += wv * irow[ix]
								}
							}
							orow[ox] = acc
						}
					}
				}
				applyPRelu(st.prelu, out, ho*wo, ch, ch+1)
			}
		}
		st.fns[0] = fn
	}
	m.par(c, fn)
}

// depthwisePlane: convoluzione depthwise a passo 1 con uscita h×w, un tap
// alla volta su tutto il piano. Il piano è contiguo, quindi uno spostamento
// (dy, dx) è uno spostamento di dy·w+dx sul vettore: una sola somma lunga
// per tap. Le colonne di bordo ricevono così i valori della riga accanto
// (il "giro" della riga): li si toglie subito dopo.
func depthwisePlane(out, in, k []float32, h, w, kh, kw, pt, pl int) {
	n := h * w
	for ky := 0; ky < kh; ky++ {
		dy := ky - pt
		y0, y1 := max(0, -dy), min(h, h-dy)
		if y1 <= y0 {
			continue
		}
		for kx := 0; kx < kw; kx++ {
			dx := kx - pl
			wv := k[ky*kw+kx]
			off := dy*w + dx
			a, b := y0*w, y1*w
			a, b = max(a, -off), min(b, n-off)
			if b <= a {
				continue
			}
			axpy(out[a:b], in[a+off:b+off], wv)
			if dx == 0 {
				continue
			}
			// colonne che hanno letto fuori dalla propria riga
			xa, xb := 0, -dx // dx < 0: colonne [0, −dx)
			if dx > 0 {
				xa, xb = w-dx, w
			}
			for y := y0; y < y1; y++ {
				for x := xa; x < xb; x++ {
					if i := y*w + x; i >= a && i < b {
						out[i] -= wv * in[i+off]
					}
				}
			}
		}
	}
}
