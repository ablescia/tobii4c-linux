package telemetry

import (
	"testing"

	"github.com/ablescia/tobii4c-linux/internal/gaze"
	"github.com/ablescia/tobii4c-linux/internal/ttp"
)

func TestBuildColumns4C(t *testing.T) {
	rows := buildColumns(sim4CColumns, gaze.DefaultKeyMap())
	type want struct {
		eye        bool
		l, lv      uint32
		r, rv      uint32
		label      string
		exported   bool
		statusFlag bool
	}
	exp := []want{
		{false, 0x01, 0, 0, 0, "Timestamp del dispositivo", true, false},
		{false, 0x11, 0, 0, 0, "Contatore 0x11, cresce di 1 a frame", false, false},
		{false, 0x14, 0, 0, 0, "Contatore dei frame del dispositivo", true, false},
		{false, 0x1c, 0x1b, 0, 0, "Sguardo combinato, filtrato", true, false},
		{true, 0x02, 0x16, 0x08, 0x15, "Centro della cornea, sistema del tracker", true, false},
		{true, 0x22, 0x21, 0x24, 0x23, "Centro della cornea, sistema dello schermo", true, false},
		{true, 0x03, 0x1d, 0x09, 0x1e, "Posizione nel track box", true, false},
		{true, 0x04, 0, 0x0a, 0, "Punto di sguardo 3D", false, false},
		{true, 0x05, 0, 0x0b, 0, "Punto di sguardo sullo schermo", true, false},
		{true, 0x06, 0, 0x0c, 0, "Diametro della pupilla", true, false},
		{true, 0x07, 0, 0x0d, 0, "Stato dell'occhio", true, true},
		{true, 0x17, 0, 0x18, 0, "Centro del bulbo oculare", false, false},
		{true, 0x19, 0, 0x1a, 0, "Posizione sul sensore", false, false},
		{false, 0x20, 0x1f, 0, 0, "Sguardo combinato, reattivo", true, false},
		{true, 0x25, 0x26, 0x27, 0x28, "Guida al posizionamento", false, false},
		{true, 0x29, 0x2a, 0x2b, 0x2c, "Apertura della palpebra", true, false},
	}
	if len(rows) != len(exp) {
		for _, r := range rows {
			t.Logf("%+v", r)
		}
		t.Fatalf("%d rows, want %d", len(rows), len(exp))
	}
	keys := 0
	for i, w := range exp {
		r := rows[i]
		got := want{r.eye, r.c[0].key, r.c[0].valid, r.c[1].key, r.c[1].valid, r.label, r.exported, r.status}
		if got != w {
			t.Errorf("row %d: %+v, want %+v", i, got, w)
		}
		for _, c := range r.c {
			for _, k := range []uint32{c.key, c.valid} {
				if k != 0 {
					keys++
				}
			}
		}
	}
	if keys != len(sim4CColumns) {
		t.Fatalf("%d columns placed, want all %d", keys, len(sim4CColumns))
	}
}

func TestFmtValue(t *testing.T) {
	p := ttp.Value{Kind: ttp.KindPoint, N: 3, F: [3]float64{-31.04, 12.5, 612.96}}
	if got := fmtValue(p, "mm"); got != " -31,0   12,5  613,0" {
		t.Fatalf("mm point %q", got)
	}
	q := ttp.Value{Kind: ttp.KindPoint, N: 2, F: [3]float64{0.5034, -1}}
	if got := fmtValue(q, "norm"); got != " 0,503 -1,000" {
		t.Fatalf("norm point %q", got)
	}
	if got := fmtValue(ttp.Value{Kind: ttp.KindFixed16, F: [3]float64{-0.0001}}, "mm"); got != "0,00" {
		t.Fatalf("negative zero %q", got)
	}
}
