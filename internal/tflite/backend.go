package tflite

import (
	"math"
	"time"
)

// Image è un frame a 8 bit: luminanza e, se CW > 0, crominanza (Cb, Cr di
// CW×CH, anche sottocampionate) come nei JPEG delle webcam.
type Image struct {
	W, H   int
	Y      []byte
	CW, CH int
	Cb, Cr []byte
}

// Gray dice se l'immagine è solo luminanza.
func (im *Image) Gray() bool { return im.CW <= 0 || im.CH <= 0 || im.Cb == nil || im.Cr == nil }

// Warp genera il tensore d'ingresso H×W×3 dall'immagine: il pixel (x, y)
// del tensore campiona l'immagine in (M0·x + M1·y + M2, M3·x + M4·y + M5)
// (centri dei pixel sugli interi, come cv::warpAffine), interpolazione
// bilineare in RGB; i valori 0..255 vanno in [Lo, Hi]. Fuori dall'immagine
// il bordo è nero o, con Replicate, il pixel più vicino.
type Warp struct {
	M         [6]float32
	Lo, Hi    float32
	Replicate bool
}

// Runner esegue un modello caricato su un backend. Non è concorrente.
type Runner interface {
	Graph() *Graph
	// RunImage genera l'ingresso con w dall'immagine dell'ultimo SetImage
	// del backend ed esegue la rete.
	RunImage(w *Warp) error
	// RunTensor esegue la rete su un ingresso già pronto (NHWC).
	RunTensor(in []float32) error
	// Output è l'uscita i dell'ultima esecuzione (valida fino alla successiva).
	Output(i int) []float32
	// Input è il tensore d'ingresso dell'ultima esecuzione (copiato dalla
	// GPU a richiesta: solo per i test e il debug).
	Input() ([]float32, error)
	// Timing sono i tempi dell'ultima esecuzione.
	Timing() Timing
	Close()
}

// Timing sono i tempi di un'esecuzione.
type Timing struct {
	Wall time.Duration // dalla chiamata al ritorno, CPU
	GPU  time.Duration // tra il primo e l'ultimo comando sulla GPU (0 sulla CPU)
}

// Backend è il luogo dove girano i modelli: la GPU (Vulkan) o la CPU.
type Backend interface {
	Name() string   // "GPU" o "CPU"
	Device() string // nome del dispositivo
	// SetImage rende im l'immagine dei RunImage successivi. Sulla GPU i
	// pixel vengono copiati; sulla CPU im va tenuta intatta fino all'ultimo
	// RunImage che la usa.
	SetImage(im *Image) error
	Load(g *Graph) (Runner, error)
	Close()
}

// sampleRGB è il pixel (ix, iy) in RGB 0..255, nero fuori dall'immagine o
// replicato dal bordo.
func sampleRGB(im *Image, ix, iy int, rep bool) (float32, float32, float32) {
	if rep {
		ix = min(max(ix, 0), im.W-1)
		iy = min(max(iy, 0), im.H-1)
	} else if ix < 0 || iy < 0 || ix >= im.W || iy >= im.H {
		return 0, 0, 0
	}
	y := float32(im.Y[iy*im.W+ix])
	if im.Gray() {
		return y, y, y
	}
	cx, cy := ix*im.CW/im.W, iy*im.CH/im.H
	cb := float32(im.Cb[cy*im.CW+cx]) - 128
	cr := float32(im.Cr[cy*im.CW+cx]) - 128
	return clamp255(y + 1.402*cr), clamp255(y - 0.344136*cb - 0.714136*cr), clamp255(y + 1.772*cb)
}

func clamp255(v float32) float32 { return min(max(v, 0), 255) }

// warpRows riempie le righe [y0, y1) del tensore dst (h×w×3) come lo
// shader warp.comp.
func warpRows(im *Image, wp *Warp, dst []float32, w, y0, y1 int) {
	m := &wp.M
	scale := (wp.Hi - wp.Lo) / 255
	for oy := y0; oy < y1; oy++ {
		for ox := 0; ox < w; ox++ {
			sx := m[0]*float32(ox) + m[1]*float32(oy) + m[2]
			sy := m[3]*float32(ox) + m[4]*float32(oy) + m[5]
			fx, fy := float32(math.Floor(float64(sx))), float32(math.Floor(float64(sy)))
			x0, y0 := int(fx), int(fy)
			dx, dy := sx-fx, sy-fy
			r00, g00, b00 := sampleRGB(im, x0, y0, wp.Replicate)
			r10, g10, b10 := sampleRGB(im, x0+1, y0, wp.Replicate)
			r01, g01, b01 := sampleRGB(im, x0, y0+1, wp.Replicate)
			r11, g11, b11 := sampleRGB(im, x0+1, y0+1, wp.Replicate)
			w00, w10, w01, w11 := (1-dx)*(1-dy), dx*(1-dy), (1-dx)*dy, dx*dy
			o := (oy*w + ox) * 3
			dst[o] = (r00*w00+r10*w10+r01*w01+r11*w11)*scale + wp.Lo
			dst[o+1] = (g00*w00+g10*w10+g01*w01+g11*w11)*scale + wp.Lo
			dst[o+2] = (b00*w00+b10*w10+b01*w01+b11*w11)*scale + wp.Lo
		}
	}
}
