package bodypose

import (
	"math"
	"sort"

	"github.com/ablescia/tobii4c-linux/internal/tflite"
)

// ROI è il ritaglio ruotato su cui gira la rete dei landmark, in pixel
// dell'immagine: centro, lati e rotazione (radianti, positiva in senso
// orario sullo schermo, come la NormalizedRect di MediaPipe).
type ROI struct {
	CX, CY, W, H, Angle float64
}

// ToImage porta un punto del ritaglio normalizzato (u, v in 0..1) nei pixel
// dell'immagine (LandmarkProjectionCalculator).
func (r ROI) ToImage(u, v float64) (float64, float64) {
	s, c := math.Sincos(r.Angle)
	x, y := (u-0.5)*r.W, (v-0.5)*r.H
	return r.CX + c*x - s*y, r.CY + s*x + c*y
}

// FromImage è l'inverso di ToImage.
func (r ROI) FromImage(px, py float64) (float64, float64) {
	s, c := math.Sincos(r.Angle)
	dx, dy := px-r.CX, py-r.CY
	return (c*dx+s*dy)/r.W + 0.5, (-s*dx+c*dy)/r.H + 0.5
}

// Warp è la trasformazione dal tensore size×size al ritaglio, come
// ImageToTensorCalculator (gli spigoli del tensore, 0 e size, cadono sugli
// spigoli del ritaglio).
func (r ROI) Warp(size int, lo, hi float32, replicate bool) tflite.Warp {
	s, c := math.Sincos(r.Angle)
	n := float64(size)
	w := tflite.Warp{Lo: lo, Hi: hi, Replicate: replicate}
	w.M = [6]float32{
		float32(c * r.W / n), float32(-s * r.H / n), float32(r.CX - 0.5*c*r.W + 0.5*s*r.H),
		float32(s * r.W / n), float32(c * r.H / n), float32(r.CY - 0.5*s*r.W - 0.5*c*r.H),
	}
	return w
}

// Corners sono gli spigoli del ritaglio nell'immagine (in senso orario dal
// primo in alto a sinistra).
func (r ROI) Corners() [4][2]float64 {
	var out [4][2]float64
	for k, uv := range [4][2]float64{{0, 0}, {1, 0}, {1, 1}, {0, 1}} {
		out[k][0], out[k][1] = r.ToImage(uv[0], uv[1])
	}
	return out
}

// roiFromPoints è AlignmentPointsRectsCalculator seguito da
// RectTransformationCalculator: centro nel primo punto, lato pari al doppio
// della distanza dal secondo (che dà anche la rotazione, a 90° dalla
// verticale), poi quadrato sul lato lungo e scalato.
func roiFromPoints(x0, y0, x1, y1, scale float64) ROI {
	size := 2 * math.Hypot(x1-x0, y1-y0)
	rot := normAngle(math.Pi/2 - math.Atan2(-(y1-y0), x1-x0))
	return ROI{CX: x0, CY: y0, W: size * scale, H: size * scale, Angle: rot}
}

func normAngle(a float64) float64 {
	return a - 2*math.Pi*math.Floor((a+math.Pi)/(2*math.Pi))
}

// Detection è una persona trovata dal detector, in pixel dell'immagine.
type Detection struct {
	Score     float64
	Box       [4]float64    // xmin, ymin, xmax, ymax
	Keypoints [4][2]float64 // centro dei fianchi, scala del corpo, centro delle spalle, scala del busto
}

// detectorConfig sono le opzioni di SsdAnchorsCalculator e
// TensorsToDetectionsCalculator dei grafi di MediaPipe.
type detectorConfig struct {
	input              int
	strides            []int
	minScale, maxScale float64
	anchors            int
}

var (
	// pose_detection_cpu.pbtxt di MediaPipe 0.10 (224×224, 2254 ancore)
	detectorFull = detectorConfig{input: 224, strides: []int{8, 16, 32, 32, 32}, minScale: 0.1484375, maxScale: 0.75, anchors: 2254}
	// lo stesso file di MediaPipe 0.8.3 (128×128, 896 ancore)
	detectorUpper = detectorConfig{input: 128, strides: []int{8, 16, 16, 16}, minScale: 0.1484375, maxScale: 0.75, anchors: 896}
)

// anchor è il centro di un'ancora (lato fisso 1: fixed_anchor_size).
type anchor struct{ x, y float64 }

// ssdAnchors genera le ancore come SsdAnchorsCalculator con
// aspect_ratios = [1], interpolated_scale_aspect_ratio = 1 e lato fisso:
// due ancore per cella per ogni strato, gli strati con lo stesso passo
// fusi.
func ssdAnchors(c detectorConfig) []anchor {
	var out []anchor
	n := len(c.strides)
	for layer := 0; layer < n; {
		last := layer
		perCell := 0
		for last < n && c.strides[last] == c.strides[layer] {
			perCell += 2 // rapporto 1 e scala interpolata
			last++
		}
		stride := c.strides[layer]
		fh := (c.input + stride - 1) / stride
		fw := fh
		for y := 0; y < fh; y++ {
			for x := 0; x < fw; x++ {
				for k := 0; k < perCell; k++ {
					out = append(out, anchor{(float64(x) + 0.5) / float64(fw), (float64(y) + 0.5) / float64(fh)})
				}
			}
		}
		layer = last
	}
	return out
}

const (
	minDetScore  = 0.5
	nmsThreshold = 0.3
)

func sigmoid(x float64) float64 { return 1 / (1 + math.Exp(-x)) }

// decodeDetections decodifica le uscite del detector (boxes: ancore × 12,
// scores: ancore) nel sistema normalizzato del tensore, poi le porta in
// pixel con il letterbox roi.
func decodeDetections(boxes, scores []float32, anchors []anchor, input int, roi ROI) []Detection {
	var dets []Detection
	sc := float64(input)
	for i, a := range anchors {
		raw := math.Max(-100, math.Min(100, float64(scores[i])))
		s := sigmoid(raw)
		if s < minDetScore {
			continue
		}
		b := boxes[i*12 : i*12+12]
		cx := float64(b[0])/sc + a.x
		cy := float64(b[1])/sc + a.y
		w := float64(b[2]) / sc
		h := float64(b[3]) / sc
		d := Detection{Score: s, Box: [4]float64{cx - w/2, cy - h/2, cx + w/2, cy + h/2}}
		for k := 0; k < 4; k++ {
			d.Keypoints[k] = [2]float64{float64(b[4+2*k])/sc + a.x, float64(b[5+2*k])/sc + a.y}
		}
		dets = append(dets, d)
	}
	dets = weightedNMS(dets)
	// dal tensore (letterbox del frame intero) ai pixel
	for i := range dets {
		d := &dets[i]
		d.Box[0], d.Box[1] = roi.ToImage(d.Box[0], d.Box[1])
		d.Box[2], d.Box[3] = roi.ToImage(d.Box[2], d.Box[3])
		for k := range d.Keypoints {
			d.Keypoints[k][0], d.Keypoints[k][1] = roi.ToImage(d.Keypoints[k][0], d.Keypoints[k][1])
		}
	}
	return dets
}

func iou(a, b [4]float64) float64 {
	ix := math.Max(0, math.Min(a[2], b[2])-math.Max(a[0], b[0]))
	iy := math.Max(0, math.Min(a[3], b[3])-math.Max(a[1], b[1]))
	inter := ix * iy
	union := (a[2]-a[0])*(a[3]-a[1]) + (b[2]-b[0])*(b[3]-b[1]) - inter
	if union <= 0 {
		return 0
	}
	return inter / union
}

// weightedNMS è NonMaxSuppressionCalculator con l'algoritmo WEIGHTED: ogni
// gruppo di riquadri sovrapposti diventa la media pesata dal punteggio
// (riquadro e punti), con il punteggio del migliore.
func weightedNMS(dets []Detection) []Detection {
	sort.SliceStable(dets, func(i, j int) bool { return dets[i].Score > dets[j].Score })
	var out []Detection
	rest := dets
	for len(rest) > 0 {
		top := rest[0]
		var keep []Detection
		var acc Detection
		total := 0.0
		for _, d := range rest {
			if iou(d.Box, top.Box) > nmsThreshold {
				total += d.Score
				for k := 0; k < 4; k++ {
					acc.Box[k] += d.Box[k] * d.Score
				}
				for k := range d.Keypoints {
					acc.Keypoints[k][0] += d.Keypoints[k][0] * d.Score
					acc.Keypoints[k][1] += d.Keypoints[k][1] * d.Score
				}
			} else {
				keep = append(keep, d)
			}
		}
		w := top
		if total > 0 {
			for k := 0; k < 4; k++ {
				w.Box[k] = acc.Box[k] / total
			}
			for k := range w.Keypoints {
				w.Keypoints[k][0] = acc.Keypoints[k][0] / total
				w.Keypoints[k][1] = acc.Keypoints[k][1] / total
			}
		}
		out = append(out, w)
		if len(keep) == len(rest) {
			break
		}
		rest = keep
	}
	return out
}

// letterbox è il ritaglio del frame intero per il detector: quadrato sul
// lato lungo, centrato, senza rotazione (ImageToTensorCalculator con
// keep_aspect_ratio e bordo nero).
func letterbox(w, h int) ROI {
	s := float64(max(w, h))
	return ROI{CX: float64(w) / 2, CY: float64(h) / 2, W: s, H: s}
}
