package irimg

import (
	"bytes"
	"testing"

	"github.com/ablescia/tobii4c-linux/internal/ttp"
)

func imageRow(w, h, bpp uint32, pix []byte) *ttp.Row {
	r := &ttp.Row{N: 5}
	r.Cols[0] = ttp.Column{Key: KeyTimestamp, Value: ttp.Value{Kind: ttp.KindS64, I: 102069393206}}
	r.Cols[1] = ttp.Column{Key: KeyBPP, Value: ttp.Value{Kind: ttp.KindU32, U: bpp}}
	r.Cols[2] = ttp.Column{Key: KeyWidth, Value: ttp.Value{Kind: ttp.KindU32, U: w}}
	r.Cols[3] = ttp.Column{Key: KeyHeight, Value: ttp.Value{Kind: ttp.KindU32, U: h}}
	r.Cols[4] = ttp.Column{Key: KeyImage, Value: ttp.Value{Kind: ttp.KindBytes, B: pix}}
	return r
}

func TestDecodeAndPGMRoundTrip(t *testing.T) {
	pix := make([]byte, 4*3)
	for i := range pix {
		pix[i] = byte(i * 20)
	}
	var f Frame
	if err := Decode(imageRow(4, 3, 8, pix), &f); err != nil {
		t.Fatal(err)
	}
	pix[0] = 99 // Decode deve aver copiato i pixel
	if f.W != 4 || f.H != 3 || f.DeviceUs != 102069393206 || f.Pix[0] != 0 || f.At(3, 2) != 220 || f.At(4, 0) != 0 {
		t.Fatalf("frame %+v", f)
	}
	var buf bytes.Buffer
	if err := f.WritePGM(&buf); err != nil {
		t.Fatal(err)
	}
	g, err := ReadPGM(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if g.W != 4 || g.H != 3 || !bytes.Equal(g.Pix, f.Pix) {
		t.Fatalf("round trip %+v", g)
	}
}

func TestDecodeRejects(t *testing.T) {
	var f Frame
	for name, r := range map[string]*ttp.Row{
		"bpp16":     imageRow(2, 2, 16, make([]byte, 8)),
		"short":     imageRow(4, 4, 8, make([]byte, 15)),
		"zero size": imageRow(0, 4, 8, nil),
	} {
		if err := Decode(r, &f); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestAppendPayloadRoundTrip(t *testing.T) {
	f := &Frame{DeviceUs: 74709605964, W: 4, H: 3, Pix: []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}}
	p := AppendPayload(nil, f)
	var row ttp.Row
	var g Frame
	if err := DecodePayload(p, &row, &g); err != nil {
		t.Fatal(err)
	}
	if g.DeviceUs != f.DeviceUs || g.W != 4 || g.H != 3 || string(g.Pix) != string(f.Pix) || row.Declared != 5 {
		t.Fatalf("round trip: %+v (declared %d)", g, row.Declared)
	}
}
