package ttp

import (
	"bytes"
	"encoding/binary"
	"math"
	"testing"
)

func TestCalibrationCommandsMatchWindowsCapture(t *testing.T) {
	cases := []struct {
		fixture string
		op, seq uint32
		payload []byte
	}{
		{"et5-calib-cmd-1030-seq48", OpCalCollect2D, 48, CalPointPayload(0.5, 0.5, EyesBoth)},
		{"et5-calib-cmd-1030-seq51", OpCalCollect2D, 51, CalPointPayload(0.5, 0.1, EyesBoth)},
		{"et5-calib-cmd-1070-seq49", OpCalCompute, 49, nil},
		{"et5-calib-cmd-1100-seq43", OpCalRead, 43, nil},
		{"et5-calib-cmd-1010-seq44", OpCalStart, 44, nil},
	}
	for _, c := range cases {
		want := fixture(t, c.fixture)
		got := AppendRequest(nil, c.op, c.seq, c.payload)
		if !bytes.Equal(got, want) {
			t.Errorf("%s:\n got %x\nwant %x", c.fixture, got, want)
		}
	}
}

func TestChunkedBlobResponse(t *testing.T) {
	// Testa reale di una risposta 1100 (blob da 659056 byte), seguita da
	// continuazioni sintetiche come le invia il dispositivo.
	head := fixture(t, "et5-calib-rsp-1100-head")
	const n = 659056
	blob := make([]byte, n)
	for i := range blob {
		blob[i] = byte(i * 7)
	}
	r := NewReassembler(1 << 16)
	var out []byte
	emit := func(m []byte) { out = append([]byte(nil), m...) }
	r.Push(head, emit)
	for off := 0; off < n; off += 16384 - 8 {
		end := min(off+16384-8, n)
		piece := make([]byte, 8, 8+end-off)
		piece[0] = 1
		binary.LittleEndian.PutUint32(piece[4:], uint32(8+end-off))
		r.Push(append(piece, blob[off:end]...), emit)
	}
	if out == nil {
		t.Fatal("chunked response not reassembled")
	}
	h, payload, err := ParseHeader(out)
	if err != nil || h.Op != OpCalRead || h.Seq != 0x2b {
		t.Fatalf("header %+v err %v", h, err)
	}
	got, err := CalBlobFromPayload(payload)
	if err != nil || !bytes.Equal(got, blob) {
		t.Fatalf("blob mismatch (len %d) err %v", len(got), err)
	}
	if !bytes.Equal(CalWritePayload(blob), payload) {
		t.Fatal("write payload must mirror the read payload framing")
	}
}

func TestParseMountingET5(t *testing.T) {
	_, payload, err := ParseHeader(fixture(t, "et5-init-rsp-2110"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := ParseMounting(payload)
	if err != nil {
		t.Fatal(err)
	}
	if m.Guides != 2 || m.WidthMM != 184 || m.AngleDeg != 20 {
		t.Fatalf("mounting %+v", m)
	}
}

func TestDisplayAreaBasicReproducesWindows(t *testing.T) {
	// Lo Stream Engine ha scritto questa display area (597x336 mm) a runtime;
	// DisplayAreaBasic con il mounting dell'ET5 deve riprodurla.
	_, mp, _ := ParseHeader(fixture(t, "et5-init-rsp-2110"))
	m, _ := ParseMounting(mp)
	msg := fixture(t, "et5-change-display-cmd-1440")
	// È un comando (dir 0x00): l'header si legge saltando l'envelope.
	want, err := ParseDisplayArea(msg[FullHeader:])
	if err != nil {
		t.Fatal(err)
	}
	got := DisplayAreaBasic(want.Width(), want.Height(), want.OffsetX(), m)
	for i, pair := range [][2][3]float64{{got.TopLeft, want.TopLeft}, {got.TopRight, want.TopRight}, {got.BottomLeft, want.BottomLeft}} {
		for k := 0; k < 3; k++ {
			if d := math.Abs(pair[0][k] - pair[1][k]); d > 1e-3 {
				t.Fatalf("corner %d axis %d: got %.6f want %.6f", i, k, pair[0][k], pair[1][k])
			}
		}
	}
	if math.Abs(want.Width()-597) > 1 || math.Abs(want.Height()-336) > 1 {
		t.Fatalf("captured area %v", want)
	}
}

// Il firmware invia ogni centro corneale in due sistemi: tracker (0x02/0x08)
// e schermo (0x22/0x24). ToDisplay, applicata con la display area della
// stessa sessione, deve riprodurre il secondo a partire dal primo.
func TestToDisplayMatchesFirmwareGazeOriginDisplay(t *testing.T) {
	_, p, err := ParseHeader(fixture(t, "et5-init-rsp-1430"))
	if err != nil {
		t.Fatal(err)
	}
	area, err := ParseDisplayArea(p)
	if err != nil {
		t.Fatal(err)
	}
	_, p, err = ParseHeader(fixture(t, "et5-session1-gaze-frame"))
	if err != nil {
		t.Fatal(err)
	}
	var row Row
	if err := DecodeRow(p, &row); err != nil {
		t.Fatal(err)
	}
	for _, k := range [][2]uint32{{0x02, 0x22}, {0x08, 0x24}} {
		tr, ok1 := row.Get(k[0])
		disp, ok2 := row.Get(k[1])
		if !ok1 || !ok2 || tr.N != 3 || disp.N != 3 {
			t.Fatalf("colonne 0x%02x/0x%02x assenti", k[0], k[1])
		}
		got := area.ToDisplay(tr.F)
		for i := range got {
			if d := math.Abs(got[i] - disp.F[i]); d > 0.01 {
				t.Errorf("0x%02x[%d]: got %.4f, firmware %.4f (Δ %.4f mm)", k[1], i, got[i], disp.F[i], d)
			}
		}
	}
}
