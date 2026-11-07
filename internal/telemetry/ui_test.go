package telemetry

import (
	"context"
	"errors"
	"image"
	"image/png"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ablescia/tobii4c-linux/internal/gaze"
	"github.com/ablescia/tobii4c-linux/internal/headpose"
	"github.com/ablescia/tobii4c-linux/internal/irimg"
	"github.com/ablescia/tobii4c-linux/internal/x11ui"
)

// fillCollector riempie un collettore con 11 s di dati.
func fillCollector(t testing.TB, now time.Time) *Collector {
	c := New(gaze.DefaultKeyMap())
	info := simSession()
	info.Since = now.Add(-83 * time.Second)
	c.SetSession(info)
	var last gaze.Sample
	n := 1000
	for i := 0; i < n; i++ {
		ts := float64(i) / 90
		host := now.Add(-time.Duration(float64(n-i) / 90 * float64(time.Second))).UnixNano()
		last = simSample(uint64(i+1), host, ts)
		c.Write(&last)
	}
	row := simRow(&last)
	c.mu.Lock()
	c.gazeRow.set(&row, last.HostTimeNs)
	for id, st := range map[uint32]StreamStat{0x500: {Count: 9000, LastSize: 1692}, 0x501: {Count: 3000, LastSize: 307350},
		0x504: {Count: 3, LastSize: 69}, 0x1772: {Count: 120, LastSize: 132}} {
		st := st
		c.streams[id] = &st
	}
	c.image = ImageMeta{W: 640, H: 480, BPP: 8, Bytes: 307200, OK: true, Columns: 5}
	c.presence = []PresenceEvent{{Time: now.Add(-95 * time.Second), State: gaze.PresenceAway}, {Time: now.Add(-61 * time.Second), State: gaze.PresencePresent, DeviceUs: 73573911340}}
	c.mu.Unlock()
	for _, l := range []LogLine{
		{Time: now.Add(-80 * time.Second), Level: 'I', Text: "subscribed stream=0x1772"},
		{Time: now.Add(-79 * time.Second), Firmware: true, Level: 'I', Text: "[ 73592.512734] (I) PROT: Client 0 qid 260 subscribing for 'log' stream (id 6002)"},
		{Time: now.Add(-40 * time.Second), Firmware: true, Level: 'I', Text: "[ 73592.529468] (I) TDI thread: Changed run mode to 22"},
		{Time: now.Add(-12 * time.Second), Level: 'W', Text: "device frames lost lost=1 total=1 counter=79001 prev=78999 device_gap_us=22222 host_gap_us=11110"},
		{Time: now.Add(-2 * time.Second), Firmware: true, Level: 'I', Text: "[ 73700.000001] (I) TDI thread: Changed run mode to 21"},
	} {
		c.AddLog(l)
	}
	return c
}

func newTestApp(c *Collector, w, h int) (*App, error) {
	a := &App{Col: c, Log: slog.New(slog.NewTextHandler(os.Stderr, nil)), meshMode: meshContours, async: make(chan func(), 16), started: true}
	return a, a.resize(w, h)
}

// setFace mette nell'anteprima un'immagine reale con i landmark del modello.
func setFace(t testing.TB, a *App, path string) {
	f, err := irimg.ReadPGMFile(path)
	if err != nil {
		t.Fatal(err)
	}
	det, err := headpose.NewDetector("", 4)
	if err != nil {
		t.Fatal(err)
	}
	var lm headpose.Landmarks
	crop, score := det.Search(f, &lm)
	a.pv.p = headpose.Preview{Seq: 1, W: f.W, H: f.H, Pix: f.Pix, Face: score >= 0.5, LM: lm, Crop: crop, Score: score, DeviceUs: f.DeviceUs}
	a.pv.have, a.pv.dirty, a.pv.lastNew = true, true, time.Now().Add(time.Hour)
	a.pv.res = headpose.Result{Score: score, InferenceMs: 11.4, Valid: true, Pitch: -0.08, RawPitch: -0.01, Yaw: 0.21, Roll: 0.03}
}

// TestRender disegna il cruscotto completo a più dimensioni. Con
// TOBII4C_TELEMETRY_PNG salva le immagini in quella cartella; con
// TOBII4C_FACE_IMAGE (PGM di `dump -images`) l'anteprima mostra un viso vero.
func TestRender(t *testing.T) {
	out := os.Getenv("TOBII4C_TELEMETRY_PNG")
	for _, sz := range [][2]int{{1880, 1000}, {1920, 1080}, {1280, 720}, {960, 1040}, {3840, 2160}, {300, 200}} {
		now := time.Now()
		c := fillCollector(t, now)
		a, err := newTestApp(c, sz[0], sz[1])
		if err != nil {
			t.Fatal(err)
		}
		a.paint(now)
		a.rates.at = now.Add(-time.Second)
		a.rates.prevStrm = map[uint32]uint64{0x500: 9000 - 90, 0x501: 3000 - 30, 0x504: 3, 0x1772: 115}
		a.rates.streamHz = map[uint32]float64{}
		a.rates.prev = counters{gaze: a.snap.Samples - 90}
		if face := os.Getenv("TOBII4C_FACE_IMAGE"); face != "" {
			setFace(t, a, face)
		}
		a.full = true
		dirty := a.paint(now.Add(time.Second))
		if len(dirty) != 1 || dirty[0] != a.canvas.Bounds() {
			t.Fatalf("%v: full paint returned %v", sz, dirty)
		}
		// un frame normale ridisegna solo le schede, dentro il canvas
		for _, r := range a.paint(now.Add(time.Second + 33*time.Millisecond)) {
			if !r.In(a.canvas.Bounds()) {
				t.Fatalf("%v: dirty rect %v outside canvas", sz, r)
			}
		}
		if out != "" {
			savePNG(t, filepath.Join(out, "telemetry-"+itoa(sz[0])+"x"+itoa(sz[1])+".png"), a.canvas)
		}
	}
}

// TestRenderPrompt disegna la domanda iniziale (servizio systemd attivo).
func TestRenderPrompt(t *testing.T) {
	c := New(gaze.DefaultKeyMap())
	a, err := newTestApp(c, 1880, 1000)
	if err != nil {
		t.Fatal(err)
	}
	a.started = false
	a.Prompt = &Prompt{Title: "Il driver è già in esecuzione come servizio",
		Lines:   []string{"Il servizio systemd tobii4c.service tiene occupato il tracker.", "Va fermato per la telemetria e verrà riavviato alla chiusura di questa finestra."},
		Confirm: "ferma il servizio", Do: func() error { return nil }}
	a.full = true
	a.paint(time.Now())
	if out := os.Getenv("TOBII4C_TELEMETRY_PNG"); out != "" {
		savePNG(t, filepath.Join(out, "telemetry-prompt.png"), a.canvas)
	}
}

func savePNG(t testing.TB, path string, img image.Image) {
	fh, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	if err := png.Encode(fh, img); err != nil {
		t.Fatal(err)
	}
	t.Log("saved", path)
}

// BenchmarkPaint misura i frame normali (schede a 30 e 10 Hz) a 1920×1080.
func BenchmarkPaint(b *testing.B) {
	now := time.Now()
	c := fillCollector(b, now)
	a, err := newTestApp(c, 1920, 1080)
	if err != nil {
		b.Fatal(err)
	}
	a.full = true
	a.paint(now)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if i%3 == 0 {
			a.pv.dirty = true // un'immagine nuova ogni frame a 30 Hz
		}
		a.paint(now)
	}
}

// Invio sulla domanda iniziale esegue l'azione fuori dal loop e, se riesce,
// avvia il tracker; se fallisce mostra l'errore e lascia riprovare.
func TestPromptFlow(t *testing.T) {
	c := New(gaze.DefaultKeyMap())
	a, err := newTestApp(c, 1280, 720)
	if err != nil {
		t.Fatal(err)
	}
	a.started = false
	a.ctx = context.Background()
	calls, started := 0, 0
	fail := true
	a.Prompt = &Prompt{Title: "t", Confirm: "ok", Do: func() error {
		calls++
		if fail {
			return errors.New("systemctl: accesso negato")
		}
		return nil
	}}
	a.Start = func() { started++ }
	enter := x11ui.Event{Kind: x11ui.KeyDown, Sym: x11ui.KeyReturn}
	runAsync := func() {
		select {
		case fn := <-a.async:
			fn()
		case <-time.After(2 * time.Second):
			t.Fatal("no result from the prompt action")
		}
	}
	a.event(enter)
	runAsync()
	if calls != 1 || started != 0 || a.Prompt == nil || a.promptErr == "" || a.promptBusy {
		t.Fatalf("after failure: calls %d started %d prompt %v err %q busy %v", calls, started, a.Prompt != nil, a.promptErr, a.promptBusy)
	}
	fail = false
	a.event(enter)
	runAsync()
	if calls != 2 || started != 1 || a.Prompt != nil || !a.Started() {
		t.Fatalf("after success: calls %d started %d prompt %v", calls, started, a.Prompt != nil)
	}
	// Esc con la domanda aperta esce senza eseguire l'azione
	b, _ := newTestApp(c, 1280, 720)
	b.Prompt = &Prompt{Do: func() error { t.Fatal("action run on Esc"); return nil }}
	b.event(x11ui.Event{Kind: x11ui.KeyDown, Sym: x11ui.KeyEscape})
	if !b.quit {
		t.Fatal("Esc did not quit")
	}
}

// Il simulatore scrive dalla sua goroutine (come framer ed emitter) mentre
// l'interfaccia copia e disegna: da eseguire con -race.
func TestCollectorConcurrent(t *testing.T) {
	c := New(gaze.DefaultKeyMap())
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		Simulate(ctx, c, nil, &irimg.Frame{W: 64, H: 48, Pix: make([]byte, 64*48)})
		close(done)
	}()
	a, err := newTestApp(c, 1280, 720)
	if err != nil {
		t.Fatal(err)
	}
	for ctx.Err() == nil {
		a.paint(time.Now())
		time.Sleep(10 * time.Millisecond)
	}
	<-done
	a.paint(time.Now())
	if !a.snap.HaveSample || !a.snap.HaveRow || a.snap.Samples < 10 || !a.snap.Image.OK || len(a.rows) != 16 {
		t.Fatalf("snapshot: sample %v row %v samples %d image %+v rows %d", a.snap.HaveSample, a.snap.HaveRow, a.snap.Samples, a.snap.Image, len(a.rows))
	}
	if a.snap.CounterStep != 1 {
		t.Fatalf("counter step %d", a.snap.CounterStep)
	}
	if len(a.snap.Presence) != 1 || a.snap.Presence[0].State != gaze.PresencePresent {
		t.Fatalf("presence %+v", a.snap.Presence)
	}
}
