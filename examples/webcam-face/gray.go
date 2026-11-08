//go:build linux

package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
)

// toGray scrive in dst (w×h byte, stride w) la luminanza del frame: è già
// l'immagine in bianco e nero, senza conversioni di colore. In YUYV è un
// byte su due; in MJPEG è il piano Y del JPEG decodificato.
func toGray(pix uint32, data []byte, w, h, bpl int, dst []byte) error {
	switch pix {
	case pixYUYV:
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
	case pixGREY:
		bpl = max(bpl, w)
		if len(data) < bpl*(h-1)+w {
			return fmt.Errorf("frame GREY troncato: %d byte su %d", len(data), bpl*h)
		}
		for y := 0; y < h; y++ {
			copy(dst[y*w:(y+1)*w], data[y*bpl:])
		}
	case pixMJPG:
		img, err := jpeg.Decode(bytes.NewReader(data))
		if err != nil {
			return fmt.Errorf("JPEG: %w", err)
		}
		b := img.Bounds()
		if b.Dx() != w || b.Dy() != h {
			return fmt.Errorf("JPEG %d×%d invece di %d×%d", b.Dx(), b.Dy(), w, h)
		}
		switch m := img.(type) {
		case *image.YCbCr:
			for y := 0; y < h; y++ {
				copy(dst[y*w:(y+1)*w], m.Y[y*m.YStride:])
			}
		case *image.Gray:
			for y := 0; y < h; y++ {
				copy(dst[y*w:(y+1)*w], m.Pix[y*m.Stride:])
			}
		default:
			for y := 0; y < h; y++ {
				for x := 0; x < w; x++ {
					dst[y*w+x] = color.GrayModel.Convert(img.At(b.Min.X+x, b.Min.Y+y)).(color.Gray).Y
				}
			}
		}
	default:
		return fmt.Errorf("formato %s non gestito", fourccString(pix))
	}
	return nil
}
