//go:build linux

// Package webcam legge le webcam UVC via V4L2 in Go puro (ioctl e mmap,
// nessuna libreria C) e ne converte i frame in piani a 8 bit: luminanza e,
// a richiesta, crominanza. Il decoder MJPEG (mjpeg.go) è parallelo sui
// marker di restart e non alloca memoria in regime.
//
// Lo usano examples/webcam-face (solo la luminanza) e examples/webcam-body
// (anche la crominanza).
package webcam

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"strconv"
	"sync/atomic"
)

// Frame sono i piani di un frame: Y di W×H e, se CW > 0, Cb e Cr di CW×CH
// (sottocampionati come li dà la webcam: 4:2:2 in YUYV e nei JPEG delle
// webcam UVC). I buffer vanno allocati con NewFrame.
type Frame struct {
	W, H   int
	Y      []byte
	CW, CH int
	Cb, Cr []byte
}

// NewFrame alloca i piani per w×h (con color anche la crominanza, fino al
// 4:4:4).
func NewFrame(w, h int, color bool) *Frame {
	f := &Frame{W: w, H: h, Y: make([]byte, w*h)}
	if color {
		f.Cb, f.Cr = make([]byte, w*h), make([]byte, w*h)
	}
	return f
}

// Color dice se il frame ha la crominanza.
func (f *Frame) Color() bool { return f.CW > 0 && f.CH > 0 }

// CopyFrom copia i piani di src (stesse dimensioni).
func (f *Frame) CopyFrom(src *Frame) {
	f.W, f.H, f.CW, f.CH = src.W, src.H, src.CW, src.CH
	f.Y = append(f.Y[:0], src.Y[:src.W*src.H]...)
	if src.Color() {
		n := src.CW * src.CH
		f.Cb = append(f.Cb[:0], src.Cb[:n]...)
		f.Cr = append(f.Cr[:0], src.Cr[:n]...)
	}
}

// Converter porta i frame della webcam in piani: YUYV e GREY con una copia,
// MJPEG con il decoder veloce, che ripiega su image/jpeg per i JPEG che non
// gestisce.
type Converter struct {
	pix       uint32
	w, h, bpl int
	mj        *mjpegDecoder
	fallback  atomic.Bool // l'ultimo frame è passato da image/jpeg
	color     atomic.Bool // l'ultimo frame aveva la crominanza
}

// NewConverter prepara la conversione del formato di in; par sono le
// goroutine della decodifica MJPEG.
func NewConverter(in *CamInfo, par int) *Converter {
	c := &Converter{pix: in.PixFmt, w: in.W, h: in.H, bpl: in.BytesPerLine}
	if c.pix == PixMJPG {
		c.mj = newMJPEGDecoder(par)
	}
	return c
}

// Gray scrive in dst (w×h byte) la luminanza del frame data.
func (c *Converter) Gray(data, dst []byte) error {
	c.color.Store(false)
	if c.mj != nil {
		err := c.mj.Decode(data, c.w, c.h, dst)
		if !errors.Is(err, errUnsupportedJPEG) {
			c.fallback.Store(false)
			return err
		}
		c.fallback.Store(true)
	}
	return toGray(c.pix, data, c.w, c.h, c.bpl, dst)
}

// Convert scrive in f i piani del frame data; con color anche la
// crominanza (f allocato con NewFrame(w, h, true)), se il formato ne ha.
func (c *Converter) Convert(data []byte, f *Frame, color bool) error {
	err := c.convert(data, f, color)
	c.color.Store(f.Color())
	return err
}

func (c *Converter) convert(data []byte, f *Frame, color bool) error {
	f.W, f.H, f.CW, f.CH = c.w, c.h, 0, 0
	if !color || f.Cb == nil {
		return c.Gray(data, f.Y)
	}
	switch c.pix {
	case PixMJPG:
		cw, ch, err := c.mj.DecodeColor(data, c.w, c.h, f.Y, f.Cb, f.Cr)
		if !errors.Is(err, errUnsupportedJPEG) {
			c.fallback.Store(false)
			f.CW, f.CH = cw, ch
			return err
		}
		c.fallback.Store(true)
		return jpegColor(data, f)
	case PixYUYV:
		bpl := max(c.bpl, 2*c.w)
		if len(data) < bpl*(c.h-1)+2*c.w {
			return fmt.Errorf("frame YUYV troncato: %d byte su %d", len(data), bpl*c.h)
		}
		cw := c.w / 2
		for y := 0; y < c.h; y++ {
			src := data[y*bpl : y*bpl+2*c.w]
			row := f.Y[y*c.w : (y+1)*c.w]
			for x := range row {
				row[x] = src[2*x]
			}
			cb, cr := f.Cb[y*cw:(y+1)*cw], f.Cr[y*cw:(y+1)*cw]
			for k := range cb {
				cb[k], cr[k] = src[4*k+1], src[4*k+3]
			}
		}
		f.CW, f.CH = cw, c.h
		return nil
	}
	return c.Gray(data, f.Y)
}

// Close ferma le goroutine della decodifica.
func (c *Converter) Close() {
	if c != nil && c.mj != nil {
		c.mj.Close()
	}
}

// Note descrive la conversione dell'ultimo frame per l'interfaccia.
func (c *Converter) Note() string {
	if c == nil {
		return ""
	}
	planes := "Y"
	if c.color.Load() {
		planes = "Y, Cb e Cr"
	}
	switch {
	case c.pix != PixMJPG:
		if planes == "Y" {
			return "solo la Y"
		}
		return "copia di " + planes
	case c.fallback.Load():
		return "JPEG → " + planes + " con image/jpeg"
	}
	return "JPEG → " + planes + " · " + strconv.Itoa(c.mj.Par()) + " goroutine"
}

// jpegColor decodifica con image/jpeg (JPEG che il decoder veloce non
// gestisce): alloca, ma è solo il ripiego.
func jpegColor(data []byte, f *Frame) error {
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("JPEG: %w", err)
	}
	b := img.Bounds()
	if b.Dx() != f.W || b.Dy() != f.H {
		return fmt.Errorf("JPEG %d×%d invece di %d×%d", b.Dx(), b.Dy(), f.W, f.H)
	}
	switch m := img.(type) {
	case *image.YCbCr:
		for y := 0; y < f.H; y++ {
			copy(f.Y[y*f.W:(y+1)*f.W], m.Y[y*m.YStride:])
		}
		if f.Cb == nil {
			return nil
		}
		cw, ch := chromaSize(m)
		for y := 0; y < ch; y++ {
			copy(f.Cb[y*cw:(y+1)*cw], m.Cb[y*m.CStride:])
			copy(f.Cr[y*cw:(y+1)*cw], m.Cr[y*m.CStride:])
		}
		f.CW, f.CH = cw, ch
	case *image.Gray:
		for y := 0; y < f.H; y++ {
			copy(f.Y[y*f.W:(y+1)*f.W], m.Pix[y*m.Stride:])
		}
	default:
		for y := 0; y < f.H; y++ {
			for x := 0; x < f.W; x++ {
				f.Y[y*f.W+x] = color.GrayModel.Convert(img.At(b.Min.X+x, b.Min.Y+y)).(color.Gray).Y
			}
		}
	}
	return nil
}

func chromaSize(m *image.YCbCr) (int, int) {
	w, h := m.Rect.Dx(), m.Rect.Dy()
	switch m.SubsampleRatio {
	case image.YCbCrSubsampleRatio422:
		return (w + 1) / 2, h
	case image.YCbCrSubsampleRatio420:
		return (w + 1) / 2, (h + 1) / 2
	case image.YCbCrSubsampleRatio440:
		return w, (h + 1) / 2
	case image.YCbCrSubsampleRatio411:
		return (w + 3) / 4, h
	case image.YCbCrSubsampleRatio410:
		return (w + 3) / 4, (h + 1) / 2
	}
	return w, h
}

// toGray scrive in dst (w×h byte, stride w) la luminanza del frame. In YUYV
// è un byte su due; in MJPEG è il piano Y del JPEG decodificato.
func toGray(pix uint32, data []byte, w, h, bpl int, dst []byte) error {
	switch pix {
	case PixYUYV:
		bpl = max(bpl, 2*w)
		if len(data) < bpl*(h-1)+2*w {
			return fmt.Errorf("frame YUYV troncato: %d byte su %d", len(data), bpl*h)
		}
		for y := 0; y < h; y++ {
			src := data[y*bpl : y*bpl+2*w]
			row := dst[y*w : (y+1)*w]
			for x := range row {
				row[x] = src[2*x]
			}
		}
	case PixGREY:
		bpl = max(bpl, w)
		if len(data) < bpl*(h-1)+w {
			return fmt.Errorf("frame GREY troncato: %d byte su %d", len(data), bpl*h)
		}
		for y := 0; y < h; y++ {
			copy(dst[y*w:(y+1)*w], data[y*bpl:])
		}
	case PixMJPG:
		f := &Frame{W: w, H: h, Y: dst}
		return jpegColor(data, f)
	default:
		return fmt.Errorf("formato %s non gestito", FourCCString(pix))
	}
	return nil
}
