// Package irimg decodifica lo stream immagine del tracker (0x501, "image").
//
// Sul 4C (firmware 4.7.0) ogni messaggio è una riga XDS di 307 350 byte a
// 30 Hz, con cinque colonne:
//
//	0x01 Timestamp       s64    µs, stesso orologio dello stream gaze
//	0x02 Bits per pixel  u32    8
//	0x03 Width           u32    640
//	0x04 Height          u32    480
//	0x05 Image           bytes  W×H pixel, riga per riga, scala di grigi (IR)
package irimg

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/ablescia/tobii4c-linux/internal/ttp"
)

// Chiavi delle colonne dello stream 0x501.
const (
	KeyTimestamp uint32 = 0x01
	KeyBPP       uint32 = 0x02
	KeyWidth     uint32 = 0x03
	KeyHeight    uint32 = 0x04
	KeyImage     uint32 = 0x05
)

// Frame è un'immagine IR a 8 bit. Pix appartiene al Frame: Decode lo
// riutilizza tra una chiamata e l'altra.
type Frame struct {
	DeviceUs int64 // timestamp del dispositivo (µs)
	HostNs   int64 // ricezione USB (CLOCK_REALTIME, ns)
	W, H     int
	Pix      []byte // W*H byte, stride W
}

// At restituisce il pixel (x, y); fuori dall'immagine vale 0.
func (f *Frame) At(x, y int) byte {
	if x < 0 || y < 0 || x >= f.W || y >= f.H {
		return 0
	}
	return f.Pix[y*f.W+x]
}

// Decode copia in f l'immagine contenuta nella riga. Accetta solo 8 bit per
// pixel, l'unico formato osservato.
func Decode(row *ttp.Row, f *Frame) error {
	var w, h, bpp uint32
	var pix []byte
	haveTS := false
	for i := 0; i < row.N; i++ {
		c := &row.Cols[i]
		switch c.Key {
		case KeyTimestamp:
			if c.Value.Kind == ttp.KindS64 {
				f.DeviceUs, haveTS = c.Value.I, true
			}
		case KeyBPP:
			bpp = c.Value.U
		case KeyWidth:
			w = c.Value.U
		case KeyHeight:
			h = c.Value.U
		case KeyImage:
			if c.Value.Kind == ttp.KindBytes {
				pix = c.Value.B
			}
		}
	}
	switch {
	case !haveTS:
		return errors.New("irimg: missing timestamp")
	case bpp != 8:
		return fmt.Errorf("irimg: unsupported bits per pixel %d", bpp)
	case w == 0 || h == 0 || w > 4096 || h > 4096:
		return fmt.Errorf("irimg: implausible size %dx%d", w, h)
	case len(pix) != int(w*h):
		return fmt.Errorf("irimg: %d bytes for %dx%d", len(pix), w, h)
	}
	f.W, f.H = int(w), int(h)
	f.Pix = append(f.Pix[:0], pix...)
	return nil
}

// DecodePayload decodifica un payload dello stream 0x501.
func DecodePayload(payload []byte, row *ttp.Row, f *Frame) error {
	if err := ttp.DecodeRow(payload, row); err != nil && row.N == 0 {
		return err
	}
	return Decode(row, f)
}

// AppendPayload codifica f come payload dello stream 0x501, nello stesso
// formato del 4C (riga XDS con timestamp, bit per pixel, larghezza, altezza e
// pixel). Serve ai test.
func AppendPayload(dst []byte, f *Frame) []byte {
	col := func(dst []byte, key uint32) []byte {
		dst = ttp.AppendTag(dst, 0x00020bb9)
		return ttp.AppendU32(dst, key)
	}
	dst = ttp.PayloadPrefix(dst)
	dst = ttp.AppendTag(dst, 5<<16|0x0bb8)
	dst = col(dst, KeyTimestamp)
	dst = append(dst, ttp.TypeS64, 0, 0, 0, 8)
	dst = binary.BigEndian.AppendUint64(dst, uint64(f.DeviceUs))
	for _, kv := range [][2]uint32{{KeyBPP, 8}, {KeyWidth, uint32(f.W)}, {KeyHeight, uint32(f.H)}} {
		dst = col(dst, kv[0])
		dst = ttp.AppendU32(dst, kv[1])
	}
	dst = col(dst, KeyImage)
	n := f.W * f.H
	dst = append(dst, ttp.TypeBytes)
	dst = binary.BigEndian.AppendUint32(dst, uint32(4+n))
	dst = binary.BigEndian.AppendUint32(dst, uint32(n))
	return append(dst, f.Pix[:n]...)
}
