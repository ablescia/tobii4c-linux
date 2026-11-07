package ttp

import (
	"encoding/hex"
	"math"
	"testing"
)

// Payload catturati dal 4C (firmware 4.7.0-d374125) il 2026-11-07.
const (
	trackBox4C   = "0000050000000400031f41040000000800025800000000000400000008000190000000000004000000080007080000000000050000000400031f410400000008fffda800000000000400000008000190000000000004000000080007080000000000050000000400031f410400000008fffda800000000000400000008fffe70000000000004000000080007080000000000050000000400031f41040000000800025800000000000400000008fffe70000000000004000000080007080000000000050000000400031f41040000000800044c0000000000040000000800032000000000000400000008000e100000000000050000000400031f410400000008fffbb40000000000040000000800032000000000000400000008000e100000000000050000000400031f410400000008fffbb400000000000400000008fffce000000000000400000008000e100000000000050000000400031f41040000000800044c00000000000400000008fffce000000000000400000008000e100000000000"
	properties4C = "0000050000000400022710020000000400000000140000000e0000000a5065726970686572616c050000000400022710020000000400000002140000000d0000000948454c4c4f5f4e49520500000004000227100200000004000000031400000017000000134953344c544f4249495045524950484552414c05000000040002271002000000040000000514000000090000000566616c736505000000040002271002000000040000000614000000090000000566616c736505000000040002271002000000040000000714000000090000000566616c736505000000040002271002000000040000000c14000000050000000133"
	log4C        = "0000050000000400010bb8050000000400020bb90200000004000000010500000004000201000200000004000033f40500000004000233f40600000008000000112274e23c140000003a000000365b2037333539322e3532393436385d2028492920544449207468726561643a204368616e6765642072756e206d6f646520746f203232"
)

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseTrackBox4C(t *testing.T) {
	tb, err := ParseTrackBox(mustHex(t, trackBox4C))
	if err != nil {
		t.Fatal(err)
	}
	want := [8][3]float64{
		{150, 100, 450}, {-150, 100, 450}, {-150, -100, 450}, {150, -100, 450},
		{275, 200, 900}, {-275, 200, 900}, {-275, -200, 900}, {275, -200, 900},
	}
	for i := range want {
		for k := range want[i] {
			if math.Abs(tb.Corners[i][k]-want[i][k]) > 1e-9 {
				t.Fatalf("corner %d: %v, want %v", i, tb.Corners[i], want[i])
			}
		}
	}
	if w, h := tb.Size(false); w != 300 || h != 200 {
		t.Fatalf("near plane %v×%v", w, h)
	}
	if w, h := tb.Size(true); w != 550 || h != 400 || tb.Near() != 450 || tb.Far() != 900 {
		t.Fatalf("far plane %v×%v, near %v far %v", w, h, tb.Near(), tb.Far())
	}
}

func TestParseProperties4C(t *testing.T) {
	got := ParseProperties(mustHex(t, properties4C))
	want := []Property{{0, "Peripheral"}, {2, "HELLO_NIR"}, {3, "IS4LTOBIIPERIPHERAL"},
		{5, "false"}, {6, "false"}, {7, "false"}, {0xc, "3"}}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("property %d: %v, want %v", i, got[i], want[i])
		}
	}
}

func TestParseLogEntries4C(t *testing.T) {
	payload := mustHex(t, log4C)
	got := ParseLogEntries(payload)
	if len(got) != 1 {
		t.Fatalf("got %d entries: %v", len(got), got)
	}
	e := got[0]
	if e.DeviceUs != 73592529468 || e.Text != "[ 73592.529468] (I) TDI thread: Changed run mode to 22" || e.Level() != 'I' {
		t.Fatalf("entry %+v level %q", e, e.Level())
	}
	// Il decoder generico vede la riga ma non il contenuto della lista.
	var row Row
	_ = DecodeRow(payload, &row)
	if row.Declared != 1 || row.N != 1 || row.Cols[0].Key != 1 || row.Cols[0].Value.Kind != KindUnknown {
		t.Fatalf("generic decode: declared %d, n %d, %+v", row.Declared, row.N, row.Cols[0])
	}
}

func TestLogEntryLevel(t *testing.T) {
	for _, c := range []struct {
		text string
		want byte
	}{{"[ 1.0] (W) x", 'W'}, {"[ 1.0] (E) y", 'E'}, {"senza livello", 0}, {"[ 1.0] (", 0}} {
		if got := (LogEntry{Text: c.text}).Level(); got != c.want {
			t.Errorf("%q: %q, want %q", c.text, got, c.want)
		}
	}
}

func TestAppendRowRoundTrip(t *testing.T) {
	var row Row
	add := func(k uint32, v Value) {
		row.Cols[row.N] = Column{Key: k, Value: v}
		row.N++
	}
	add(0x01, Value{Kind: KindS64, I: 74709605964})
	add(0x07, Value{Kind: KindU32, U: 4})
	add(0x06, Value{Kind: KindFixed16, F: [3]float64{-1}})
	add(0x1c, Value{Kind: KindPoint, N: 2, F: [3]float64{0.5, -1}})
	add(0x02, Value{Kind: KindPoint, N: 3, F: [3]float64{-31.25, 95.5, 610.125}})
	add(0x05, Value{Kind: KindBytes, B: []byte{1, 2, 3}})
	add(0x09, Value{Kind: KindFixed42, F: [3]float64{0.25}})
	var got Row
	if err := DecodeRow(AppendRow(nil, &row), &got); err != nil {
		t.Fatal(err)
	}
	if got.N != row.N || got.Declared != row.N {
		t.Fatalf("n %d declared %d", got.N, got.Declared)
	}
	for i := 0; i < row.N; i++ {
		a, b := row.Cols[i], got.Cols[i]
		if a.Key != b.Key || a.Value.Kind != b.Value.Kind || a.Value.U != b.Value.U || a.Value.I != b.Value.I ||
			a.Value.F != b.Value.F || a.Value.N != b.Value.N || string(a.Value.B) != string(b.Value.B) {
			t.Fatalf("column %d: %+v, want %+v", i, b, a)
		}
	}
	// lo stesso frame gaze della cattura ET5, ricodificato, è identico byte per byte
	_, payload, err := ParseHeader(fixture(t, "et5-session1-gaze-frame"))
	if err != nil {
		t.Fatal(err)
	}
	var et5 Row
	if err := DecodeRow(payload, &et5); err != nil {
		t.Fatal(err)
	}
	if enc := AppendRow(nil, &et5); string(enc) != string(payload) {
		t.Fatalf("re-encoded ET5 frame differs (%d vs %d bytes)", len(enc), len(payload))
	}
}
