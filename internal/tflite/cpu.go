package tflite

import (
	"errors"
	"fmt"
	"math"
	"runtime"
	"sync"
	"time"

	"github.com/ablescia/tobii4c-linux/internal/nn"
)

// CPU è il backend di ripiego e di riferimento: gli stessi nodi della GPU
// in Go, con i kernel AVX2/FMA di internal/nn per le convoluzioni 1×1.
// In regime un'esecuzione non alloca memoria.
type CPU struct {
	img     *Image
	workers int
	tasks   chan cpuTask
	wg      sync.WaitGroup
	once    sync.Once
}

type cpuTask struct {
	fn     func(lo, hi int)
	lo, hi int
}

// NewCPU prepara il backend con workers goroutine (0: una per core, al più 16).
func NewCPU(workers int) *CPU {
	if workers <= 0 {
		workers = min(runtime.NumCPU(), 16)
	}
	return &CPU{workers: workers}
}

func (c *CPU) Name() string { return "CPU" }

func (c *CPU) Device() string {
	s := fmt.Sprintf("%d goroutine", c.workers)
	if nn.HasAVX2() {
		s += " · AVX2/FMA"
	}
	return s
}

// Workers è il numero di goroutine di calcolo.
func (c *CPU) Workers() int { return c.workers }

func (c *CPU) SetImage(im *Image) error {
	if im == nil || im.W <= 0 || im.H <= 0 || len(im.Y) < im.W*im.H {
		return errors.New("tflite: immagine non valida")
	}
	if !im.Gray() && (len(im.Cb) < im.CW*im.CH || len(im.Cr) < im.CW*im.CH) {
		return errors.New("tflite: crominanza non valida")
	}
	c.img = im
	return nil
}

func (c *CPU) Close() {
	if c.tasks != nil {
		close(c.tasks)
		c.tasks = nil
	}
}

func (c *CPU) start() {
	c.tasks = make(chan cpuTask, 4*c.workers)
	for i := 0; i < c.workers; i++ {
		go func() {
			for t := range c.tasks {
				t.fn(t.lo, t.hi)
				c.wg.Done()
			}
		}()
	}
}

// par divide [0, n) in parti per le goroutine; grain è il lavoro minimo per
// parte (in unità di n) sotto il quale non conviene dividere.
func (c *CPU) par(n, grain int, fn func(lo, hi int)) {
	parts := min(c.workers, max(1, n/max(grain, 1)))
	if parts <= 1 {
		fn(0, n)
		return
	}
	c.once.Do(c.start)
	chunk := (n + parts - 1) / parts
	for lo := 0; lo < n; lo += chunk {
		c.wg.Add(1)
		c.tasks <- cpuTask{fn, lo, min(lo+chunk, n)}
	}
	c.wg.Wait()
}

type cpuRunner struct {
	c     *CPU
	g     *Graph
	arena []float32
	fns   []func(lo, hi int)
	ns    []int // dimensione del lavoro di ogni nodo
	grain []int
	warp  Warp
	wfn   func(lo, hi int)
	tm    Timing
}

func (c *CPU) Load(g *Graph) (Runner, error) {
	r := &cpuRunner{c: c, g: g, arena: make([]float32, g.Arena)}
	g.WeightData(r.arena)
	for i := range g.Nodes {
		fn, n, grain := r.kernel(&g.Nodes[i])
		r.fns, r.ns, r.grain = append(r.fns, fn), append(r.ns, n), append(r.grain, grain)
	}
	is := g.InputShape()
	h, w := is[1], is[2]
	in := r.arena[g.Off(g.Input) : g.Off(g.Input)+h*w*is[3]]
	r.wfn = func(lo, hi int) { warpRows(c.img, &r.warp, in, w, lo, hi) }
	return r, nil
}

func (r *cpuRunner) Graph() *Graph  { return r.g }
func (r *cpuRunner) Close()         {}
func (r *cpuRunner) Timing() Timing { return r.tm }

func (r *cpuRunner) RunImage(w *Warp) error {
	if r.c.img == nil {
		return errors.New("tflite: nessuna immagine")
	}
	t0 := time.Now()
	is := r.g.InputShape()
	if is[3] != 3 {
		return fmt.Errorf("tflite: ingresso con %d canali, l'immagine ne dà 3", is[3])
	}
	r.warp = *w
	r.c.par(is[1], 8, r.wfn)
	r.exec()
	r.tm = Timing{Wall: time.Since(t0)}
	return nil
}

func (r *cpuRunner) RunTensor(in []float32) error {
	n := r.g.Values[r.g.Input].Elems()
	if len(in) != n {
		return fmt.Errorf("tflite: ingresso di %d float, attesi %d", len(in), n)
	}
	t0 := time.Now()
	copy(r.arena[r.g.Off(r.g.Input):], in)
	r.exec()
	r.tm = Timing{Wall: time.Since(t0)}
	return nil
}

func (r *cpuRunner) exec() {
	for i := range r.fns {
		r.c.par(r.ns[i], r.grain[i], r.fns[i])
	}
}

func (r *cpuRunner) Output(i int) []float32 {
	v := r.g.Outputs[i]
	o := r.g.Off(v)
	return r.arena[o : o+r.g.Values[v].Elems()]
}

func (r *cpuRunner) Input() ([]float32, error) {
	o := r.g.Off(r.g.Input)
	return append([]float32(nil), r.arena[o:o+r.g.Values[r.g.Input].Elems()]...), nil
}

func actf(v float32, m int) float32 {
	switch m {
	case ActRelu:
		return max(v, 0)
	case ActRelu1:
		return min(max(v, -1), 1)
	case ActRelu6:
		return min(max(v, 0), 6)
	case ActTanh:
		return float32(math.Tanh(float64(v)))
	case ActLogistic:
		return float32(1 / (1 + math.Exp(-float64(v))))
	}
	return v
}

// kernel restituisce la funzione di un nodo su un intervallo del suo
// lavoro, la dimensione del lavoro e la granularità minima.
func (r *cpuRunner) kernel(n *Node) (func(lo, hi int), int, int) {
	g, A := r.g, r.arena
	view := func(v int) []float32 {
		o := g.Off(v)
		return A[o : o+g.Values[v].Elems()]
	}
	out := view(n.Out)
	os := g.Values[n.Out].Shape
	var wt, bias, res []float32
	if n.W >= 0 {
		wt = view(n.W)
	}
	if n.B >= 0 {
		bias = view(n.B)
	}
	if n.Res >= 0 {
		res = view(n.Res)
	}
	finish := func(row []float32, base int) {
		for i := range row {
			v := actf(row[i], n.Act)
			if res != nil {
				v = actf(v+res[base+i], n.Act2)
			}
			row[i] = v
		}
	}
	switch n.Kind {
	case KPointwise:
		x := view(n.In[0])
		P, K, N := g.GemmShape(n)
		return func(lo, hi int) {
			var w4 [4]float32
			for p := lo; p < hi; p++ {
				row := out[p*N : (p+1)*N]
				if bias != nil {
					copy(row, bias)
				} else {
					clear(row)
				}
				xr := x[p*K : (p+1)*K]
				k := 0
				for ; k+4 <= K; k += 4 {
					w4 = [4]float32{xr[k], xr[k+1], xr[k+2], xr[k+3]}
					nn.Axpy4(row, wt[k*N:], wt[(k+1)*N:], wt[(k+2)*N:], wt[(k+3)*N:], &w4)
				}
				for ; k < K; k++ {
					nn.Axpy(row, wt[k*N:], xr[k])
				}
				finish(row, p*N)
			}
		}, P, max(1, 4096/max(K*N/64, 1))
	case KConv:
		x := view(n.In[0])
		is := g.Values[n.In[0]].Shape
		IH, IW, C := is[1], is[2], is[3]
		OH, OW, N := os[1], os[2], os[3]
		return func(lo, hi int) {
			for oy := lo; oy < hi; oy++ {
				for ox := 0; ox < OW; ox++ {
					row := out[(oy*OW+ox)*N : (oy*OW+ox+1)*N]
					if bias != nil {
						copy(row, bias)
					} else {
						clear(row)
					}
					for kh := 0; kh < n.KH; kh++ {
						iy := oy*n.SH + kh*n.DH - n.PT
						if iy < 0 || iy >= IH {
							continue
						}
						for kw := 0; kw < n.KW; kw++ {
							ix := ox*n.SW + kw*n.DW - n.PL
							if ix < 0 || ix >= IW {
								continue
							}
							xi := x[(iy*IW+ix)*C : (iy*IW+ix+1)*C]
							wb := (kh*n.KW + kw) * C * N
							for c, xv := range xi {
								nn.Axpy(row, wt[wb+c*N:], xv)
							}
						}
					}
					finish(row, (oy*OW+ox)*N)
				}
			}
		}, OH, 1
	case KDepthwise:
		x := view(n.In[0])
		is := g.Values[n.In[0]].Shape
		IH, IW, C := is[1], is[2], is[3]
		OH, OW := os[1], os[2]
		return func(lo, hi int) {
			for oy := lo; oy < hi; oy++ {
				for ox := 0; ox < OW; ox++ {
					row := out[(oy*OW+ox)*C : (oy*OW+ox+1)*C]
					if bias != nil {
						copy(row, bias)
					} else {
						clear(row)
					}
					for kh := 0; kh < n.KH; kh++ {
						iy := oy*n.SH + kh*n.DH - n.PT
						if iy < 0 || iy >= IH {
							continue
						}
						for kw := 0; kw < n.KW; kw++ {
							ix := ox*n.SW + kw*n.DW - n.PL
							if ix < 0 || ix >= IW {
								continue
							}
							xi := x[(iy*IW+ix)*C : (iy*IW+ix+1)*C]
							wi := wt[(kh*n.KW+kw)*C : (kh*n.KW+kw+1)*C]
							for c := range row {
								row[c] += xi[c] * wi[c]
							}
						}
					}
					finish(row, (oy*OW+ox)*C)
				}
			}
		}, OH, 1
	case KPool:
		x := view(n.In[0])
		is := g.Values[n.In[0]].Shape
		IH, IW, C := is[1], is[2], is[3]
		OH, OW := os[1], os[2]
		return func(lo, hi int) {
			for oy := lo; oy < hi; oy++ {
				for ox := 0; ox < OW; ox++ {
					for c := 0; c < C; c++ {
						s := float32(0)
						if n.Max {
							s = -math.MaxFloat32
						}
						cnt := 0
						for kh := 0; kh < n.KH; kh++ {
							iy := oy*n.SH + kh - n.PT
							if iy < 0 || iy >= IH {
								continue
							}
							for kw := 0; kw < n.KW; kw++ {
								ix := ox*n.SW + kw - n.PL
								if ix < 0 || ix >= IW {
									continue
								}
								v := x[(iy*IW+ix)*C+c]
								if n.Max {
									s = max(s, v)
								} else {
									s += v
								}
								cnt++
							}
						}
						if !n.Max && cnt > 0 {
							s /= float32(cnt)
						}
						out[(oy*OW+ox)*C+c] = actf(s, n.Act)
					}
				}
			}
		}, OH, 1
	case KBinary:
		a, b := view(n.In[0]), view(n.In[1])
		nb := n.NB
		return func(lo, hi int) {
			for i := lo; i < hi; i++ {
				var v float32
				switch n.BinOp {
				case BinAdd:
					v = a[i] + b[i%nb]
				case BinMul:
					v = a[i] * b[i%nb]
				case BinSub:
					v = a[i] - b[i%nb]
				case BinDiv:
					v = a[i] / b[i%nb]
				}
				out[i] = actf(v, n.Act)
			}
		}, len(out), 16384
	case KUnary:
		a := view(n.In[0])
		return func(lo, hi int) {
			for i := lo; i < hi; i++ {
				out[i] = actf(a[i], n.Act)
			}
		}, len(out), 16384
	case KPad:
		x := view(n.In[0])
		is := g.Values[n.In[0]].Shape
		IH, IW, IC := is[1], is[2], is[3]
		OH, OW, OC := os[1], os[2], os[3]
		return func(lo, hi int) {
			for oy := lo; oy < hi; oy++ {
				iy := oy - n.Pad[1]
				for ox := 0; ox < OW; ox++ {
					ix := ox - n.Pad[2]
					row := out[(oy*OW+ox)*OC : (oy*OW+ox+1)*OC]
					if iy < 0 || iy >= IH || ix < 0 || ix >= IW {
						for c := range row {
							row[c] = n.PadValue
						}
						continue
					}
					for c := range row {
						ic := c - n.Pad[3]
						if ic < 0 || ic >= IC {
							row[c] = n.PadValue
						} else {
							row[c] = x[(iy*IW+ix)*IC+ic]
						}
					}
				}
			}
		}, OH, 1
	case KResize:
		x := view(n.In[0])
		is := g.Values[n.In[0]].Shape
		IH, IW, C := is[1], is[2], is[3]
		OH, OW := os[1], os[2]
		sy, sx := resizeScale(IH, OH, n.AlignCorners), resizeScale(IW, OW, n.AlignCorners)
		return func(lo, hi int) {
			for oy := lo; oy < hi; oy++ {
				fy := resizeCoord(oy, sy, n.HalfPixel)
				y0, y1 := max(int(math.Floor(float64(fy))), 0), min(int(math.Ceil(float64(fy))), IH-1)
				dy := fy - float32(y0)
				for ox := 0; ox < OW; ox++ {
					fx := resizeCoord(ox, sx, n.HalfPixel)
					x0, x1 := max(int(math.Floor(float64(fx))), 0), min(int(math.Ceil(float64(fx))), IW-1)
					dx := fx - float32(x0)
					o := (oy*OW + ox) * C
					for c := 0; c < C; c++ {
						v00 := x[(y0*IW+x0)*C+c]
						v01 := x[(y0*IW+x1)*C+c]
						v10 := x[(y1*IW+x0)*C+c]
						v11 := x[(y1*IW+x1)*C+c]
						out[o+c] = v00*(1-dy)*(1-dx) + v10*dy*(1-dx) + v01*(1-dy)*dx + v11*dy*dx
					}
				}
			}
		}, OH, 1
	case KDepthToSpace:
		x := view(n.In[0])
		is := g.Values[n.In[0]].Shape
		IW, IC := is[2], is[3]
		bs := n.Block
		OH, OW, OC := os[1], os[2], os[3]
		return func(lo, hi int) {
			for oy := lo; oy < hi; oy++ {
				for ox := 0; ox < OW; ox++ {
					src := ((oy/bs)*IW + ox/bs) * IC
					ic := ((oy%bs)*bs + ox%bs) * OC
					copy(out[(oy*OW+ox)*OC:(oy*OW+ox+1)*OC], x[src+ic:src+ic+OC])
				}
			}
		}, OH, 1
	case KConcat:
		outer, inner := 1, 1
		for a := 0; a < n.Axis; a++ {
			outer *= os[a]
		}
		for a := n.Axis + 1; a < len(os); a++ {
			inner *= os[a]
		}
		ins := make([][]float32, len(n.In))
		widths := make([]int, len(n.In))
		for k, v := range n.In {
			ins[k] = view(v)
			widths[k] = g.Values[v].Shape[n.Axis] * inner
		}
		outA := os[n.Axis] * inner
		return func(lo, hi int) {
			for o := lo; o < hi; o++ {
				off := 0
				for k, src := range ins {
					w := widths[k]
					copy(out[o*outA+off:o*outA+off+w], src[o*w:(o+1)*w])
					off += w
				}
			}
		}, outer, 64
	}
	panic("tflite: nodo " + n.Kind.String() + " senza kernel CPU")
}

func resizeScale(in, out int, align bool) float32 {
	if align && out > 1 {
		return float32(in-1) / float32(out-1)
	}
	return float32(in) / float32(out)
}

func resizeCoord(o int, s float32, half bool) float32 {
	if half {
		return (float32(o)+0.5)*s - 0.5
	}
	return float32(o) * s
}
