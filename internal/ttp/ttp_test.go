package ttp

import (
	"bytes"
	"encoding/hex"
	"math"
	"os"
	"strings"
	"testing"
)

func fixture(t testing.TB, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name + ".hex")
	if err != nil {
		t.Fatal(err)
	}
	b, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestAppendRequestHelloLayout(t *testing.T) {
	// Il frame hello catturato dallo Stream Engine Windows (79 byte).
	want, _ := hex.DecodeString("0000000047000000000000510000000100000000000003e8000000000000002f" +
		"0000170000002800000009000100000001000100010002000100030001000400010005000100060001000700010008")
	got := AppendRequest(nil, OpHello, 1, HelloPayload())
	if !bytes.Equal(got, want) {
		t.Fatalf("hello frame mismatch\n got %x\nwant %x", got, want)
	}
}

func TestStreamStartPayload(t *testing.T) {
	// Ultime righe di init_packets_ep.txt (Krazubu): start dello stream 0x500.
	want, _ := hex.DecodeString("0000020000000400000500170000000400000000")
	if got := StreamStartPayload(StreamGaze); !bytes.Equal(got, want) {
		t.Fatalf("got %x want %x", got, want)
	}
}

func TestParseHeaderResponse(t *testing.T) {
	msg := fixture(t, "et5-init-rsp-1000")
	h, payload, err := ParseHeader(msg)
	if err != nil {
		t.Fatal(err)
	}
	if h.Marker != MarkerResponse || h.Op != OpHello || h.Seq != 1 || h.Status != 1 || h.Error != 0 {
		t.Fatalf("header %+v", h)
	}
	if int(h.PayloadLen) != len(payload) {
		t.Fatalf("plen %d vs %d", h.PayloadLen, len(payload))
	}
	if v := ParseU32s(payload); len(v) != 1 || v[0] != 0x00010008 {
		t.Fatalf("protocol version %x", v)
	}
}

func TestParseCatalogue(t *testing.T) {
	_, payload, err := ParseHeader(fixture(t, "et5-init-rsp-1200"))
	if err != nil {
		t.Fatal(err)
	}
	recs, err := ParseList(payload)
	if err != nil {
		t.Fatal(err)
	}
	want := map[uint32]string{0x500: "gaze", 0x501: "image", 0x504: "presence", 0x508: "image_collection",
		0x50e: "primary_camera_image", 0x1770: "algodbg", 0x1771: "is5_sync_stream", 0x1772: "log", 0x1774: "custom"}
	if len(recs) != len(want) {
		t.Fatalf("got %d records: %v", len(recs), recs)
	}
	for _, r := range recs {
		if want[r.ID] != r.Name {
			t.Errorf("record %v: want name %q", r, want[r.ID])
		}
	}
}

func TestParseColumns(t *testing.T) {
	_, payload, _ := ParseHeader(fixture(t, "et5-columns-rsp-1210-0x1770"))
	recs, err := ParseList(payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].ID != 1 || recs[0].Name != "Debug data" {
		t.Fatalf("columns %v", recs)
	}
}

func TestParseDeviceStrings(t *testing.T) {
	_, payload, _ := ParseHeader(fixture(t, "et5-init-rsp-1420"))
	s := ParseStrings(payload)
	if len(s) != 4 || s[2] != "IS5" || s[1] != "IS5_Large_Eyetracker_5" {
		t.Fatalf("strings %q", s)
	}
}

func TestParseDisplayAreaAndRoundTrip(t *testing.T) {
	_, payload, _ := ParseHeader(fixture(t, "et5-init-rsp-1430"))
	d, err := ParseDisplayArea(payload)
	if err != nil {
		t.Fatal(err)
	}
	if d.Width() < 100 || d.Width() > 1000 || d.Height() < 100 || d.Height() > 1000 {
		t.Fatalf("implausible display area %v", d)
	}
	// Il payload SET deve contenere gli stessi tre punti in testa.
	set := DisplayAreaSetPayload(d)
	if len(set) != 164 {
		t.Fatalf("set payload len %d", len(set))
	}
	back, err := ParseDisplayArea(set)
	if err != nil || back != d {
		t.Fatalf("roundtrip %v vs %v (%v)", back, d, err)
	}
}

func TestDecodeRealGazeFrame(t *testing.T) {
	msg := fixture(t, "et5-session1-gaze-frame")
	h, payload, err := ParseHeader(msg)
	if err != nil || h.Marker != MarkerStream || h.Op != StreamGaze {
		t.Fatalf("header %+v err %v", h, err)
	}
	var row Row
	if err := DecodeRow(payload, &row); err != nil {
		t.Fatal(err)
	}
	if row.Declared != 39 || row.N != 39 {
		t.Fatalf("columns declared %d decoded %d", row.Declared, row.N)
	}
	// Valori di riferimento: ciò che lo Stream Engine Windows ha consegnato
	// per questo frame (Krazubu/tobii-linux, gaze83.rs).
	ts, _ := row.Get(0x01)
	if ts.Kind != KindS64 || ts.I != 9_613_320_391 {
		t.Fatalf("timestamp %+v", ts)
	}
	fc, _ := row.Get(0x14)
	if fc.U != 43_780 {
		t.Fatalf("frame counter %d", fc.U)
	}
	g, _ := row.Get(0x1c)
	if g.Kind != KindPoint || g.N != 2 ||
		g.F[0] != 0.5872913599014282 || g.F[1] != 0.09927384555339813 {
		t.Fatalf("gaze point %+v", g)
	}
	lo, _ := row.Get(0x22)
	if lo.F != [3]float64{-57.900634765625, 112.46868896484375, 618.2794189453125} {
		t.Fatalf("left origin %+v", lo.F)
	}
	p, _ := row.Get(0x06)
	if p.Kind != KindFixed16 || p.F[0] <= 1 || p.F[0] >= 9 || math.IsNaN(p.F[0]) {
		t.Fatalf("pupil %+v", p)
	}
}

func TestReassemblerSplitAndMerged(t *testing.T) {
	a := fixture(t, "et5-session1-gaze-frame")
	b := fixture(t, "et5-init-presence")
	c := fixture(t, "et5-init-rsp-1000")
	stream := append(append(append([]byte{}, a...), b...), c...)

	for _, chunk := range []int{1, 7, 64, 512, 1000, len(stream)} {
		r := NewReassembler(1 << 16)
		var got [][]byte
		for off := 0; off < len(stream); off += chunk {
			end := min(off+chunk, len(stream))
			r.Push(stream[off:end], func(m []byte) { got = append(got, append([]byte(nil), m...)) })
		}
		if len(got) != 3 || !bytes.Equal(got[0], a) || !bytes.Equal(got[1], b) || !bytes.Equal(got[2], c) {
			t.Fatalf("chunk %d: got %d messages", chunk, len(got))
		}
	}
}

func TestReassemblerResync(t *testing.T) {
	a := fixture(t, "et5-init-presence")
	junk := []byte{0xde, 0xad, 0xbe, 0xef, 0x01, 0x00, 0x00, 0x00, 0x99, 0x99, 0x99, 0x99, 0x42}
	r := NewReassembler(4096)
	n := 0
	r.Push(append(append([]byte{}, junk...), a...), func(m []byte) {
		if !bytes.Equal(m, a) {
			t.Fatalf("bad message after resync")
		}
		n++
	})
	if n != 1 || r.Resyncs == 0 {
		t.Fatalf("n=%d resyncs=%d", n, r.Resyncs)
	}
}

func TestReassemblerChunkedResponse(t *testing.T) {
	// Risposta che dichiara 20 byte di payload ma ne trasporta 4; il resto
	// arriva in due continuazioni (envelope senza header).
	body := []byte("0123456789abcdefghij")
	head := []byte{1, 0, 0, 0, 36, 0, 0, 0,
		0, 0, 0, 0x52, 0, 0, 0, 9, 0, 0, 0, 1, 0, 0, 0x04, 0x4c, 0, 0, 0, 0, 0, 0, 0, 20}
	head = append(head, body[:4]...)
	cont1 := append([]byte{1, 0, 0, 0, 8 + 10, 0, 0, 0}, body[4:14]...)
	cont2 := append([]byte{1, 0, 0, 0, 8 + 6, 0, 0, 0}, body[14:]...)
	r := NewReassembler(4096)
	var out []byte
	for _, p := range [][]byte{head, cont1, cont2} {
		r.Push(p, func(m []byte) { out = append([]byte(nil), m...) })
	}
	h, payload, err := ParseHeader(out)
	if err != nil || h.Seq != 9 || !bytes.Equal(payload, body) {
		t.Fatalf("h=%+v payload=%q err=%v", h, payload, err)
	}
}

func TestDecodeRowZeroAlloc(t *testing.T) {
	_, payload, _ := ParseHeader(fixture(t, "et5-session1-gaze-frame"))
	var row Row
	allocs := testing.AllocsPerRun(1000, func() { _ = DecodeRow(payload, &row) })
	if allocs != 0 {
		t.Fatalf("DecodeRow allocates %.1f times per call", allocs)
	}
}

func TestReassemblerZeroAlloc(t *testing.T) {
	msg := fixture(t, "et5-session1-gaze-frame")
	r := NewReassembler(1 << 16)
	n := 0
	emit := func([]byte) { n++ }
	allocs := testing.AllocsPerRun(1000, func() { r.Push(msg, emit) })
	if allocs != 0 {
		t.Fatalf("Push allocates %.1f times per call", allocs)
	}
}

func BenchmarkDecodeGazeRow(b *testing.B) {
	_, payload, _ := ParseHeader(fixture(b, "et5-session1-gaze-frame"))
	var row Row
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	for i := 0; i < b.N; i++ {
		if err := DecodeRow(payload, &row); err != nil {
			b.Fatal(err)
		}
	}
}
