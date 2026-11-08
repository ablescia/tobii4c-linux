//go:build linux

// Comando webcam-face: l'anteprima dei punti del viso della telemetria del
// driver (tobii4c telemetry), ma sulle immagini di una webcam invece che
// sul sensore IR del Tobii 4C. Usa lo stesso modello (MediaPipe Face
// Landmarker incorporato nel driver) con lo stesso interprete ONNX in Go
// puro; la webcam si legge via V4L2 senza librerie C.
//
// A schermo intero: a sinistra l'immagine in bianco e nero (la luminanza
// della webcam) con i 478 punti, a destra la telemetria di webcam, modello,
// viso e processo.
//
//	go run ./examples/webcam-face
//	go run ./examples/webcam-face -size 1280x720 -format mjpeg
//	go run ./examples/webcam-face -png /tmp/viso.png   # senza finestra
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ablescia/tobii4c-linux/internal/headpose"
	"github.com/ablescia/tobii4c-linux/internal/x11ui"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "webcam-face:", err)
		os.Exit(1)
	}
}

func run() error {
	device := flag.String("device", "auto", "nodo V4L2 della webcam; auto: quella USB con -usb-id, altrimenti la prima")
	usbID := flag.String("usb-id", "1bcf:2281", "vid:pid cercato da -device auto (Sunplus SPCA2281)")
	size := flag.String("size", "640x480", "risoluzione richiesta alla webcam (il driver può sceglierne una vicina)")
	format := flag.String("format", "yuyv", "formato: yuyv (luminanza senza decodifica), mjpeg (risoluzioni alte a 30 fps), grey")
	fps := flag.Int("fps", 30, "frame al secondo richiesti")
	buffers := flag.Int("buffers", 4, "buffer mmap del driver")
	fov := flag.Float64("fov", 60, "campo visivo orizzontale della webcam, gradi: dà la focale per posa e distanze")
	model := flag.String("model", "", "modello ONNX dei landmark (default: quello incorporato nel driver)")
	workers := flag.Int("workers", 1, "goroutine dell'inferenza")
	mirror := flag.Bool("mirror", true, "anteprima a specchio (tasto V)")
	monName := flag.String("monitor", "", "monitor della finestra (default: il primario)")
	windowed := flag.Bool("window", false, "finestra normale invece dello schermo intero (tasto F)")
	shots := flag.String("shots", ".", "cartella delle schermate (tasto S)")
	pngOut := flag.String("png", "", "senza finestra: dopo -duration salva l'interfaccia in questo PNG ed esce")
	pngSize := flag.String("png-size", "1920x1080", "dimensione dell'interfaccia per -png")
	duration := flag.Duration("duration", 0, "esce dopo questo tempo (con -png: default 4s)")
	flag.Parse()

	w, h, err := parseSize(*size)
	if err != nil {
		return fmt.Errorf("-size: %w", err)
	}
	var pix uint32
	switch strings.ToLower(*format) {
	case "yuyv":
		pix = pixYUYV
	case "mjpeg", "mjpg":
		pix = pixMJPG
	case "grey", "gray":
		pix = pixGREY
	default:
		return fmt.Errorf("-format: %q non valido (yuyv, mjpeg, grey)", *format)
	}
	if !(*fov > 1 && *fov < 179) {
		return fmt.Errorf("-fov: %v non valido", *fov)
	}

	path := *device
	if path == "auto" {
		if path, err = findWebcam(*usbID); err != nil {
			return err
		}
	}
	cam, err := OpenWebcam(path, w, h, pix, *fps, *buffers)
	if err != nil {
		return err
	}
	defer cam.Close()
	det, err := headpose.NewDetector(*model, *workers)
	if err != nil {
		return err
	}
	name := "incorporato"
	if *model != "" {
		name = *model
	}
	// focale pinhole dal campo visivo orizzontale
	focal := float64(cam.Info.W) / 2 / math.Tan(*fov/2*math.Pi/180)
	eng := NewEngine(cam, det, focal, *fov, max(1, *workers), name)
	fmt.Fprintf(os.Stderr, "webcam-face: %s (%s) %d×%d %s, %.1f fps nominali\n", path, cam.Info.Card,
		cam.Info.W, cam.Info.H, fourccString(cam.Info.PixFmt), cam.Info.FPS)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if *pngOut != "" && *duration == 0 {
		*duration = 4 * time.Second
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
	win, err := x11ui.OpenWindow(conn, x11ui.WindowOptions{Title: "tobii4c · viso dalla webcam", Class: "tobii4c-webcam-face",
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
	fmt.Fprintln(os.Stderr, "webcam-face: interfaccia salvata in", path)
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
