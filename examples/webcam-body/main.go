//go:build linux

// Comando webcam-body: lo scheletro di una persona dalla webcam con i
// modelli di Google MediaPipe Pose, eseguiti sulla GPU con Vulkan (o sulla
// CPU) da un interprete TFLite in Go puro (internal/tflite), senza librerie
// C né runtime di Google.
//
// Un solo programma per le due tipologie di rilevamento, scelte con i radio
// button in alto a sinistra (o con il tasto B):
//
//   - Corpo intero, in piedi o seduto: MediaPipe Pose Landmarker (lite, full
//     o heavy), 33 punti con le coordinate 3D in metri e la maschera della
//     persona; dai punti 3D la postura (in piedi, seduto, accovacciato,
//     sdraiato) e gli angoli delle articolazioni.
//   - Mezzo busto, da seduto: il modello "upper body" di MediaPipe Pose
//     0.8.3, addestrato sulla sola parte superiore del corpo: 25 punti
//     (testa, spalle, braccia, mani, fianchi) e la maschera.
//
// I modelli vengono scaricati la prima volta dai server di Google (SHA-256
// verificato) in ~/.cache/tobii4c/models.
//
// A schermo intero: a sinistra i comandi, l'immagine a colori con lo
// scheletro e i grafici; a destra la telemetria di postura, articolazioni,
// scheletro 3D, modello, GPU, webcam e processo.
//
//	go run ./examples/webcam-body
//	go run ./examples/webcam-body -mode busto       # mezzo busto
//	go run ./examples/webcam-body -variant heavy    # corpo intero, modello pesante
//	go run ./examples/webcam-body -backend cpu      # senza GPU
//	go run ./examples/webcam-body -png /tmp/corpo.png   # senza finestra
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ablescia/tobii4c-linux/internal/bodypose"
	"github.com/ablescia/tobii4c-linux/internal/tflite"
	"github.com/ablescia/tobii4c-linux/internal/webcam"
	"github.com/ablescia/tobii4c-linux/internal/x11ui"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "webcam-body:", err)
		os.Exit(1)
	}
}

func run() error {
	device := flag.String("device", "auto", "nodo V4L2 della webcam; auto: la prima USB con un id di -usb-id, altrimenti la prima")
	usbID := flag.String("usb-id", "046d:085b,1bcf:2281", "vid:pid cercati da -device auto, in ordine (Logitech C925e, Sunplus SPCA2281)")
	size := flag.String("size", "auto", "risoluzione: auto (la più grande alla frequenza scelta) o LARGHEZZAxALTEZZA")
	format := flag.String("format", "auto", "formato: auto, yuyv, mjpeg, grey (grey: senza colore)")
	fps := flag.Float64("fps", 0, "frame al secondo; 0: i più possibili")
	fixedFPS := flag.Bool("fixed-fps", true, "frequenza fissa anche con poca luce (spegne exposure_dynamic_framerate, ripristinato all'uscita)")
	ctrlFlag := flag.String("ctrl", "", "controlli V4L2 nome=valore separati da virgole, coi nomi di v4l2-ctl; ripristinati all'uscita")
	list := flag.Bool("list", false, "elenca le modalità della webcam e quella scelta, poi esce")
	buffers := flag.Int("buffers", 4, "buffer mmap del driver")
	decoders := flag.Int("decoders", 0, "goroutine della decodifica MJPEG; 0: automatico (un quarto dei core, al più 8)")
	mode := flag.String("mode", "corpo", "tipologia di rilevamento: corpo (corpo intero) o busto (mezzo busto, da seduto)")
	variant := flag.String("variant", bodypose.Full, "modello del corpo intero: lite, full, heavy")
	backend := flag.String("backend", "gpu", "dove eseguire i modelli: gpu (Vulkan, con ripiego sulla CPU) o cpu")
	workers := flag.Int("workers", 0, "goroutine dei modelli sulla CPU; 0: una per core, al più 16")
	models := flag.String("models", "", "cartella dei modelli (default: "+bodypose.DefaultDir()+")")
	offline := flag.Bool("offline", false, "non scaricare i modelli mancanti")
	fov := flag.Float64("fov", 70, "campo visivo orizzontale della webcam, gradi: dà la focale per la distanza")
	mirror := flag.Bool("mirror", true, "anteprima a specchio (tasto V)")
	monName := flag.String("monitor", "", "monitor della finestra (default: il primario)")
	windowed := flag.Bool("window", false, "finestra normale invece dello schermo intero (tasto F)")
	shots := flag.String("shots", ".", "cartella delle schermate (tasto S)")
	pngOut := flag.String("png", "", "senza finestra: dopo -duration salva l'interfaccia in questo PNG ed esce")
	pngSize := flag.String("png-size", "1920x1080", "dimensione dell'interfaccia per -png")
	duration := flag.Duration("duration", 0, "esce dopo questo tempo (con -png: default 5s)")
	flag.Parse()

	var cfg Config
	switch strings.ToLower(*mode) {
	case "corpo", "body", "full":
		cfg.Mode = bodypose.FullBody
	case "busto", "halfbody", "upper", "mezzo":
		cfg.Mode = bodypose.UpperBody
	default:
		return fmt.Errorf("-mode: %q non valido (corpo, busto)", *mode)
	}
	if _, err := bodypose.SpecFor(bodypose.FullBody, *variant); err != nil {
		return fmt.Errorf("-variant: %w", err)
	}
	cfg.Variant = *variant
	switch strings.ToLower(*backend) {
	case "gpu", "vulkan":
		cfg.GPU = true
	case "cpu":
	default:
		return fmt.Errorf("-backend: %q non valido (gpu, cpu)", *backend)
	}

	var req webcam.ModeRequest
	if !strings.EqualFold(*size, "auto") {
		w, h, err := parseSize(*size)
		if err != nil {
			return fmt.Errorf("-size: %w", err)
		}
		req.W, req.H = w, h
	}
	switch strings.ToLower(*format) {
	case "auto":
	case "yuyv":
		req.PixFmt = webcam.PixYUYV
	case "mjpeg", "mjpg":
		req.PixFmt = webcam.PixMJPG
	case "grey", "gray":
		req.PixFmt = webcam.PixGREY
	default:
		return fmt.Errorf("-format: %q non valido (auto, yuyv, mjpeg, grey)", *format)
	}
	if *fps < 0 || math.IsNaN(*fps) {
		return fmt.Errorf("-fps: %v non valido", *fps)
	}
	req.FPS = *fps
	ctrls, err := webcam.ParseCtrlSettings(*ctrlFlag)
	if err != nil {
		return fmt.Errorf("-ctrl: %w", err)
	}
	if !(*fov > 1 && *fov < 179) {
		return fmt.Errorf("-fov: %v non valido", *fov)
	}
	nd := *decoders
	if nd <= 0 {
		nd = min(max(runtime.NumCPU()/4, 1), 8)
	}

	path := *device
	if path == "auto" {
		if path, err = webcam.FindWebcam(*usbID); err != nil {
			return err
		}
	}
	if *list {
		return webcam.ListModes(path, req)
	}

	// prima i modelli (possono essere da scaricare), poi la webcam
	logf := func(f string, a ...any) { fmt.Fprintf(os.Stderr, "webcam-body: "+f+"\n", a...) }
	progress := func(msg string) { logf("%s", msg) }
	if err := bodypose.Ensure(cfg.Mode, cfg.Variant, *models, *offline, progress); err != nil {
		return err
	}
	var gpu *tflite.GPU
	gpuErr := ""
	if g, err := tflite.NewGPU(); err != nil {
		gpuErr = err.Error()
		if cfg.GPU {
			logf("GPU non disponibile (%v): uso la CPU", err)
			cfg.GPU = false
		}
	} else {
		gpu = g
		in := g.Info()
		logf("GPU: %s · %s %s · Vulkan %s", in.Name, in.Driver, in.DriverInfo, in.API)
	}
	cpu := tflite.NewCPU(*workers)

	cam, err := webcam.Open(path, webcam.OpenOptions{Mode: req, Buffers: *buffers, FixedFPS: *fixedFPS, Controls: ctrls})
	if err != nil {
		if gpu != nil {
			gpu.Close()
		}
		cpu.Close()
		return err
	}
	defer cam.Close()
	// focale pinhole dal campo visivo orizzontale
	focal := float64(cam.Info.W) / 2 / math.Tan(*fov/2*math.Pi/180)
	eng := NewEngine(cam, gpu, gpuErr, cpu, focal, *fov, nd, *models, *offline)
	defer eng.Close()
	if err := eng.Prepare(cfg, progress); err != nil {
		return err
	}
	logf("%s (%s) %d×%d %s, %.1f fps nominali · %s", path, cam.Info.Card,
		cam.Info.W, cam.Info.H, webcam.FourCCString(cam.Info.PixFmt), cam.Info.FPS, cam.Info.Choice)
	logf("rilevamento: %s", cfg.key())
	for _, ch := range cam.Info.Changed {
		logf("%s %d → %d (ripristinato all'uscita)", ch.Name, ch.Old, ch.New)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if *pngOut != "" && *duration == 0 {
		*duration = 5 * time.Second
	}
	if *duration > 0 {
		ctx, cancel = context.WithTimeout(ctx, *duration)
		defer cancel()
	}
	engDone := make(chan struct{})
	engCtx, stopEng := context.WithCancel(context.Background())
	go func() { eng.Run(engCtx); close(engDone) }()
	defer func() { stopEng(); <-engDone }()

	if *pngOut != "" {
		return renderPNG(ctx, eng, *pngOut, *pngSize, *mirror)
	}

	conn, err := x11ui.Connect()
	if err != nil {
		return err
	}
	defer conn.Close()
	mons, err := x11ui.Monitors(conn)
	if err != nil {
		return err
	}
	if len(mons) == 0 {
		return errors.New("nessun monitor")
	}
	mon := mons[0]
	for _, m := range mons {
		if (*monName == "" && m.Primary) || (*monName != "" && strings.EqualFold(m.Name, *monName)) {
			mon = m
		}
	}
	if *monName != "" && !strings.EqualFold(mon.Name, *monName) {
		return fmt.Errorf("monitor %q non trovato", *monName)
	}
	win, err := x11ui.OpenWindow(conn, x11ui.WindowOptions{Title: "tobii4c · scheletro dalla webcam", Class: "tobii4c-webcam-body",
		X: mon.X, Y: mon.Y, W: mon.W, H: mon.H, MinW: 960, MinH: 540, Background: 0x45494f, Fullscreen: !*windowed})
	if err != nil {
		return err
	}
	defer win.Close()
	app := NewApp(win, eng, win.W, win.H, *mirror)
	app.ShotDir = *shots
	return app.Run(ctx)
}

// renderPNG disegna l'interfaccia in memoria, senza X: utile per provarla a
// distanza o senza display.
func renderPNG(ctx context.Context, eng *Engine, path, size string, mirror bool) error {
	w, h, err := parseSize(size)
	if err != nil {
		return fmt.Errorf("-png-size: %w", err)
	}
	app := NewApp(nil, eng, w, h, mirror)
	if err := app.resize(w, h); err != nil {
		return err
	}
	tk := time.NewTicker(time.Second / 30)
	defer tk.Stop()
	for done := false; !done; {
		select {
		case <-ctx.Done():
			done = true
		case now := <-tk.C:
			app.paint(now)
		}
	}
	app.full = true
	app.paint(time.Now())
	if err := savePNG(path, app.canvas); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "webcam-body: interfaccia salvata in", path)
	return nil
}

func parseSize(s string) (int, int, error) {
	a, b, ok := strings.Cut(strings.ToLower(s), "x")
	if !ok {
		return 0, 0, fmt.Errorf("%q: serve LARGHEZZAxALTEZZA", s)
	}
	w, err1 := strconv.Atoi(a)
	h, err2 := strconv.Atoi(b)
	if err1 != nil || err2 != nil || w <= 0 || h <= 0 {
		return 0, 0, fmt.Errorf("%q: serve LARGHEZZAxALTEZZA", s)
	}
	return w, h, nil
}
