//go:build linux

package webcam

import (
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

// Modalità della webcam e controlli impostati all'apertura.

// Mode è una modalità di cattura: formato, risoluzione e tempi per frame
// possibili, dal più breve.
type Mode struct {
	PixFmt uint32
	W, H   int
	Ivals  []v4l2Fract
}

func fractFPS(f v4l2Fract) float64 {
	if f.Numerator == 0 {
		return 0
	}
	return float64(f.Denominator) / float64(f.Numerator)
}

// MaxFPS è la frequenza più alta della modalità.
func (m Mode) MaxFPS() float64 {
	if len(m.Ivals) == 0 {
		return 0
	}
	return fractFPS(m.Ivals[0])
}

// formatCost ordina i formati che si sanno portare in luminanza, a parità di
// risoluzione e frequenza: GREY e YUYV non vanno decodificati.
var formatCost = map[uint32]int{PixGREY: 0, PixYUYV: 1, PixMJPG: 2}

// enumModes elenca formati, risoluzioni e frequenze (ENUM_FMT,
// ENUM_FRAMESIZES, ENUM_FRAMEINTERVALS). Delle risoluzioni continue o a
// passi tiene la più grande, degli intervalli continui gli estremi.
func enumModes(fd int) []Mode {
	var out []Mode
	for fi := uint32(0); fi < 64; fi++ {
		d := v4l2FmtDesc{Index: fi, Type: bufTypeVideoCapture}
		if ioctl(fd, vidiocEnumFmt, unsafe.Pointer(&d)) != nil {
			break
		}
		for si := uint32(0); si < 256; si++ {
			fs := v4l2FrmSizeEnum{Index: si, PixelFormat: d.PixelFormat}
			if ioctl(fd, vidiocEnumFrameSizes, unsafe.Pointer(&fs)) != nil {
				break
			}
			w, h := fs.U[0], fs.U[1]
			if fs.Type != frmTypeDiscrete {
				w, h = fs.U[1], fs.U[4]
			}
			out = append(out, Mode{PixFmt: d.PixelFormat, W: int(w), H: int(h), Ivals: enumIntervals(fd, d.PixelFormat, w, h)})
			if fs.Type != frmTypeDiscrete {
				break
			}
		}
	}
	return out
}

func enumIntervals(fd int, pix, w, h uint32) []v4l2Fract {
	var out []v4l2Fract
	for i := uint32(0); i < 64; i++ {
		fi := v4l2FrmIvalEnum{Index: i, PixelFormat: pix, Width: w, Height: h}
		if ioctl(fd, vidiocEnumFrameIntervals, unsafe.Pointer(&fi)) != nil {
			break
		}
		out = append(out, v4l2Fract{fi.U[0], fi.U[1]})
		if fi.Type != frmTypeDiscrete {
			out = append(out, v4l2Fract{fi.U[2], fi.U[3]})
			break
		}
	}
	keep := out[:0]
	for _, f := range out {
		if f.Numerator > 0 && f.Denominator > 0 {
			keep = append(keep, f)
		}
	}
	sort.SliceStable(keep, func(i, j int) bool { return fractFPS(keep[i]) > fractFPS(keep[j]) })
	return keep
}

// ModeRequest è la modalità chiesta; gli zeri lasciano la scelta a pickMode.
type ModeRequest struct {
	W, H   int     // 0: qualsiasi
	PixFmt uint32  // 0: qualsiasi formato convertibile in luminanza
	FPS    float64 // 0: la più alta
}

func iabs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// pickMode sceglie tra le modalità: prima la frequenza più alta (o la più
// vicina a quella chiesta), poi la risoluzione più grande, poi il formato
// più economico da convertire. Con una risoluzione chiesta che la webcam
// non ha considera le più vicine. Restituisce anche il tempo per frame da
// chiedere.
func pickMode(modes []Mode, req ModeRequest) (Mode, v4l2Fract, bool) {
	var cand []Mode
	for _, m := range modes {
		if _, ok := formatCost[m.PixFmt]; !ok || len(m.Ivals) == 0 || m.W <= 0 || m.H <= 0 {
			continue
		}
		if req.PixFmt != 0 && m.PixFmt != req.PixFmt {
			continue
		}
		cand = append(cand, m)
	}
	if req.W > 0 && req.H > 0 {
		best := math.MaxInt
		for _, m := range cand {
			best = min(best, iabs(m.W-req.W)+iabs(m.H-req.H))
		}
		keep := cand[:0]
		for _, m := range cand {
			if iabs(m.W-req.W)+iabs(m.H-req.H) == best {
				keep = append(keep, m)
			}
		}
		cand = keep
	}
	var (
		bm    Mode
		bi    v4l2Fract
		bs    float64
		found bool
	)
	for _, m := range cand {
		iv := m.Ivals[0]
		score := fractFPS(iv)
		if req.FPS > 0 {
			for _, x := range m.Ivals[1:] {
				if math.Abs(fractFPS(x)-req.FPS) < math.Abs(fractFPS(iv)-req.FPS) {
					iv = x
				}
			}
			score = -math.Abs(fractFPS(iv) - req.FPS)
		}
		px, bpx := m.W*m.H, bm.W*bm.H
		better := !found || score > bs+1e-6 ||
			score > bs-1e-6 && (px > bpx || px == bpx && formatCost[m.PixFmt] < formatCost[bm.PixFmt])
		if better {
			bm, bi, bs, found = m, iv, score, true
		}
	}
	return bm, bi, found
}

func choiceText(req ModeRequest, n int) string {
	if req == (ModeRequest{}) {
		return "automatica, la più veloce tra " + strconv.Itoa(n)
	}
	var by []string
	if req.W > 0 {
		by = append(by, "-size")
	}
	if req.PixFmt != 0 {
		by = append(by, "-format")
	}
	if req.FPS > 0 {
		by = append(by, "-fps")
	}
	return "la migliore con " + strings.Join(by, " ") + " tra " + strconv.Itoa(n)
}

// fpsList è "60 30 24 … fps".
func fpsList(m Mode) string {
	var b strings.Builder
	for i, f := range m.Ivals {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(strconv.FormatFloat(fractFPS(f), 'f', -1, 64))
	}
	b.WriteString(" fps")
	return b.String()
}

// ListModes stampa le modalità della webcam e quella che verrebbe scelta.
func ListModes(path string, req ModeRequest) error {
	if !isCaptureNode(path) {
		return fmt.Errorf("%s: non è un nodo di cattura video in streaming", path)
	}
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	defer syscall.Close(fd)
	modes := enumModes(fd)
	if len(modes) == 0 {
		return fmt.Errorf("%s: la webcam non elenca le modalità", path)
	}
	best, iv, ok := pickMode(modes, req)
	for _, m := range modes {
		mark := "  "
		if ok && m.PixFmt == best.PixFmt && m.W == best.W && m.H == best.H {
			mark = "→ "
		}
		note := ""
		if _, conv := formatCost[m.PixFmt]; !conv {
			note = " (formato non convertibile)"
		}
		fmt.Printf("%s%-4s %5d×%-5d %s%s\n", mark, FourCCString(m.PixFmt), m.W, m.H, fpsList(m), note)
	}
	if ok {
		fmt.Printf("scelta: %s %d×%d a %s fps\n", FourCCString(best.PixFmt), best.W, best.H,
			strconv.FormatFloat(fractFPS(iv), 'f', -1, 64))
	}
	return nil
}

// ---------------------------------------------------------------------------
// Controlli

// CtrlSetting è un controllo da impostare (-ctrl nome=valore).
type CtrlSetting struct{ Name, Value string }

// CtrlChange è un controllo cambiato all'apertura.
type CtrlChange struct {
	ID       uint32
	Name     string
	Old, New int32
}

// ParseCtrlSettings legge "nome=valore,nome=valore".
func ParseCtrlSettings(s string) ([]CtrlSetting, error) {
	var out []CtrlSetting
	for _, kv := range strings.Split(s, ",") {
		kv = strings.TrimSpace(kv)
		if kv == "" {
			continue
		}
		k, v, ok := strings.Cut(kv, "=")
		if !ok || strings.TrimSpace(k) == "" || strings.TrimSpace(v) == "" {
			return nil, fmt.Errorf("%q: serve nome=valore", kv)
		}
		out = append(out, CtrlSetting{strings.TrimSpace(k), strings.TrimSpace(v)})
	}
	return out, nil
}

// ctrlKey è il nome di un controllo come lo scrive v4l2-ctl:
// "Exposure Time, Absolute" → exposure_time_absolute.
func ctrlKey(name string) string {
	var b strings.Builder
	sep := false
	for _, r := range strings.ToLower(name) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			if sep && b.Len() > 0 {
				b.WriteByte('_')
			}
			sep = false
			b.WriteRune(r)
		} else {
			sep = true
		}
	}
	return b.String()
}

func findCtrl(cs []Control, name string) *Control {
	key := ctrlKey(name)
	id, err := strconv.ParseUint(name, 0, 32)
	for i := range cs {
		if c := &cs[i]; !c.Class && (ctrlKey(c.Name) == key || err == nil && uint64(c.ID) == id) {
			return c
		}
	}
	return nil
}

// parseCtrlValue interpreta il valore: un numero, on/off per i booleani o la
// voce di un menu (con lo stesso nome di v4l2-ctl, es. 50_hz).
func parseCtrlValue(c *Control, s string) (int32, error) {
	var v int64
	if n, err := strconv.ParseInt(s, 0, 32); err == nil {
		v = n
	} else {
		switch strings.ToLower(s) {
		case "on", "true", "yes", "si", "sì":
			v = 1
		case "off", "false", "no":
			v = 0
		default:
			found := false
			var names []string
			for i, m := range c.Menu {
				if ctrlKey(m) == ctrlKey(s) {
					v, found = int64(i), true
				}
				names = append(names, ctrlKey(m))
			}
			if !found {
				sort.Strings(names)
				if len(names) == 0 {
					return 0, fmt.Errorf("%s: valore %q non numerico", ctrlKey(c.Name), s)
				}
				return 0, fmt.Errorf("%s: valore %q non valido (%s)", ctrlKey(c.Name), s, strings.Join(names, ", "))
			}
		}
	}
	if v < int64(c.Min) || v > int64(c.Max) {
		return 0, fmt.Errorf("%s: %d fuori da %d…%d", ctrlKey(c.Name), v, c.Min, c.Max)
	}
	return int32(v), nil
}

// setCtrl imposta un controllo e ne annota il valore originale per Close
// (se era leggibile).
func (c *Webcam) setCtrl(ct *Control, v int32) error {
	old, known := ct.Value, ct.OK
	x := v4l2Control{ID: ct.ID, Value: v}
	if err := ioctl(c.fd, vidiocSCtrl, unsafe.Pointer(&x)); err != nil {
		return fmt.Errorf("%s=%d: %w", ctrlKey(ct.Name), v, err)
	}
	g := v4l2Control{ID: ct.ID}
	if ioctl(c.fd, vidiocGCtrl, unsafe.Pointer(&g)) == nil {
		v = g.Value
	}
	ct.Value = v
	for i := range c.Info.Changed {
		if ch := &c.Info.Changed[i]; ch.ID == ct.ID {
			ch.New = v
			return nil
		}
	}
	if known && old != v {
		c.Info.Changed = append(c.Info.Changed, CtrlChange{ID: ct.ID, Name: ctrlKey(ct.Name), Old: old, New: v})
	}
	return nil
}

// applyControls fissa la frequenza (se chiesto) e imposta i controlli di
// -ctrl; con FixedFPS un errore è solo un avviso.
func (c *Webcam) applyControls(opt OpenOptions) error {
	if !opt.FixedFPS && len(opt.Controls) == 0 {
		return nil
	}
	cs := c.Controls()
	if opt.FixedFPS {
		for i := range cs {
			if ct := &cs[i]; ct.ID == cidExposureAutoPriority && ct.OK && ct.Value != 0 {
				if err := c.setCtrl(ct, 0); err != nil {
					fmt.Fprintln(os.Stderr, "webcam-face: frequenza non fissata:", err)
				}
			}
		}
	}
	for _, s := range opt.Controls {
		ct := findCtrl(cs, s.Name)
		if ct == nil {
			var names []string
			for _, x := range cs {
				if !x.Class {
					names = append(names, ctrlKey(x.Name))
				}
			}
			return fmt.Errorf("controllo %q non trovato (disponibili: %s)", s.Name, strings.Join(names, ", "))
		}
		v, err := parseCtrlValue(ct, s.Value)
		if err != nil {
			return err
		}
		if err := c.setCtrl(ct, v); err != nil {
			return err
		}
	}
	return nil
}
