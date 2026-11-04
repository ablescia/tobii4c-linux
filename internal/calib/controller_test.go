package calib

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"math"
	"os"
	"testing"
	"time"

	"github.com/ablescia/tobii4c-linux/internal/store"
	"github.com/ablescia/tobii4c-linux/internal/ttp"
)

// setup prepara controller + simulatore accelerato, con ~/.config isolata.
func setup(t *testing.T) (*Controller, *SimDevice, context.Context) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg := DefaultConfig()
	cfg.FocusSettle = 80 * time.Millisecond
	cfg.NoGazeDwell = 150 * time.Millisecond
	// La dinamica dello sguardo simulato non è accelerata da TimeScale
	// (latenza saccadica 180 ms, costante di tempo 60 ms): con meno di ~400 ms
	// la media della finestra include ancora la transizione dal punto precedente.
	cfg.ValidationSettle = 450 * time.Millisecond
	cfg.ValidationWindow = 300 * time.Millisecond
	c := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	sim := NewSimDevice(c.Hub)
	sim.TimeScale = 0.02
	sim.OnNotify = c.Notify
	c.SetDevice(sim)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go sim.Run(ctx)
	time.Sleep(50 * time.Millisecond) // primi campioni nel hub
	return c, sim, ctx
}

// maxAttempts è lo stesso limite di internal/calibui: un punto perso viene
// ripresentato. Col simulatore accelerato la raccolta dura ~14 ms, quindi un
// ammiccamento (120 ms in tempo reale) può coprirla tutta e dare lost_eyes;
// sul 4C reale la raccolta dura ~665 ms e lo stesso ammiccamento resta sotto
// la soglia del 50%.
const maxAttempts = 3

func calibrate(t *testing.T, c *Controller, ctx context.Context) {
	t.Helper()
	if err := c.Begin(); err != nil {
		t.Fatal(err)
	}
	for _, batch := range ttp.CalibrationBatches7 {
		for _, i := range batch {
			p := ttp.CalibrationPoints7[i]
			c.Show(float64(p[0]), float64(p[1]), true)
			var o Outcome
			collecting := false
			for attempt := 0; attempt < maxAttempts; attempt++ {
				collecting = false
				o = c.Collect(ctx, float64(p[0]), float64(p[1]), func() { collecting = true })
				if o.OK || o.Reason != "lost_eyes" {
					break
				}
			}
			// Il primo punto arriva dopo il clear, senza sguardo: sosta fissa
			// (come sul 4C reale), non l'attesa fino al timeout. Gli altri,
			// dopo il primo compute, si confermano con lo sguardo.
			wantMode := "focus"
			if i == 0 {
				wantMode = "dwell"
			}
			if !o.OK || o.Mode != wantMode || !collecting || o.FocusTime > time.Second {
				t.Fatalf("point %v: want mode %s, got %+v", p, wantMode, o)
			}
		}
		if _, err := c.Compute(); err != nil {
			t.Fatal(err)
		}
	}
}

func meanError(t *testing.T, c *Controller, ctx context.Context) float64 {
	t.Helper()
	var sum float64
	for _, p := range ValidationPoints {
		c.Show(p[0], p[1], true)
		r, err := c.Validate(ctx, p[0], p[1])
		if err != nil || r.N == 0 {
			t.Fatalf("validate %v: %+v %v", p, r, err)
		}
		sum += r.ErrorDeg
	}
	return sum / float64(len(ValidationPoints))
}

func TestFullCalibrationFlow(t *testing.T) {
	c, sim, ctx := setup(t)
	before := meanError(t, c, ctx)
	original, _ := sim.ReadCalibration()

	calibrate(t, c, ctx)
	if got := c.CalibrationID(); got != 3 {
		t.Fatalf("calibration id from notifications = %d, want 3 (one per compute)", got)
	}
	st, err := c.Finish()
	if err != nil || st.Active || !st.Finished || st.SavedPath == "" {
		t.Fatalf("finish: %+v %v", st, err)
	}
	saved, err := os.ReadFile(st.SavedPath)
	if err != nil || len(saved) == 0 {
		t.Fatalf("saved calibration: %v", err)
	}
	backup, err := os.ReadFile(st.BackupPath)
	if err != nil || !bytes.Equal(backup, original) {
		t.Fatal("the backup must hold the calibration active before the run")
	}
	after := meanError(t, c, ctx)
	if !(after < before/2) || after > 1.0 {
		t.Fatalf("accuracy did not improve: before %.2f° after %.2f°", before, after)
	}
	if math.IsNaN(after) {
		t.Fatal("NaN accuracy")
	}

	// Revert rimette sul dispositivo e nel file la calibrazione precedente.
	if !c.CanRevert() {
		t.Fatal("revert must be possible after a finished calibration")
	}
	if err := c.Revert(); err != nil {
		t.Fatal(err)
	}
	now, _ := sim.ReadCalibration()
	p, _ := store.CalibrationPath()
	file, _ := os.ReadFile(p)
	if !bytes.Equal(now, original) || !bytes.Equal(file, original) {
		t.Fatal("revert did not restore the previous calibration")
	}
}

func TestAbortRestoresPrevious(t *testing.T) {
	c, sim, ctx := setup(t)
	original, _ := sim.ReadCalibration()
	if err := c.Begin(); err != nil {
		t.Fatal(err)
	}
	c.Show(0.5, 0.5, true)
	if o := c.Collect(ctx, 0.5, 0.5, nil); !o.OK {
		t.Fatalf("%+v", o)
	}
	// Una display area non si può scrivere a calibrazione aperta.
	if _, err := c.ApplyDisplay(698, 393, 0, "x"); err == nil {
		t.Fatal("display area must be refused during calibration")
	}
	restored, err := c.Abort()
	if err != nil || !restored {
		t.Fatalf("abort: restored=%v err=%v", restored, err)
	}
	if st := c.State(); st.Active {
		t.Fatal("still active after abort")
	}
	now, _ := sim.ReadCalibration()
	if !bytes.Equal(now, original) {
		t.Fatal("abort did not restore the backup")
	}
}

func TestNoEyesIsReported(t *testing.T) {
	c, _, ctx := setup(t)
	c.cfg.FocusTimeout = 200 * time.Millisecond
	if err := c.Begin(); err != nil {
		t.Fatal(err)
	}
	// Nessun campione recente: hub fermo simulando l'assenza dell'utente.
	c.Hub = &Hub{}
	o := c.Collect(ctx, 0.5, 0.5, nil)
	if o.OK || o.Reason != "no_eyes" {
		t.Fatalf("want no_eyes, got %+v", o)
	}
}

func TestApplyDisplayUsesMounting(t *testing.T) {
	c, _, _ := setup(t)
	area, err := c.ApplyDisplay(698, 393, 12, "V32F390")
	if err != nil {
		t.Fatal(err)
	}
	// Bordo inferiore come calcolato dallo Stream Engine col mounting del 4C.
	if math.Abs(area.BottomLeft[1]-16.86) > 0.01 || math.Abs(area.BottomLeft[2]+1.91) > 0.01 {
		t.Fatalf("bottom edge %v", area.BottomLeft)
	}
	if math.Abs(area.Width()-698) > 1e-6 || math.Abs(area.OffsetX()-12) > 1e-6 {
		t.Fatalf("area %v", area)
	}
	d, err := store.LoadDisplay()
	if err != nil || d.WidthMM != 698 || d.Monitor != "V32F390" {
		t.Fatalf("display.json %+v %v", d, err)
	}
}
