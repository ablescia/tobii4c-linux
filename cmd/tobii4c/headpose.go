package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"image/color"
	"image/png"
	"io/fs"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ablescia/tobii4c-linux/internal/headpose"
	"github.com/ablescia/tobii4c-linux/internal/irimg"
	"github.com/ablescia/tobii4c-linux/internal/store"

	"image"
	_ "image/jpeg"
)

// dataDir è la cartella dei dati installati ($XDG_DATA_HOME/tobii4c,
// default ~/.local/share/tobii4c): ci sta il modello del viso.
func dataDir() string {
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "tobii4c")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "tobii4c")
}

func defaultModelPath() string { return filepath.Join(dataDir(), headpose.ModelFileName) }

// headposeFlags sono i flag del pitch della testa (comando run).
type headposeFlags struct {
	mode    string
	model   string
	workers int
	rate    float64
	debug   string
	camera  string
	always  bool
}

func (h *headposeFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&h.mode, "headpose", "auto", "pitch della testa dalle immagini IR: auto (se il modello è installato), on, off")
	fs.StringVar(&h.model, "headpose-model", defaultModelPath(), "modello ONNX dei landmark del viso (make headpose-model)")
	fs.IntVar(&h.workers, "headpose-workers", 1, "goroutine dell'inferenza del modello del viso (più goroutine: meno latenza, più CPU)")
	fs.Float64Var(&h.rate, "headpose-rate", 30, "inferenze al secondo al massimo (le immagini arrivano a 30 Hz)")
	fs.StringVar(&h.debug, "headpose-debug", "", "cartella in cui salvare ogni 2 s un'immagine annotata (landmark, ritaglio, occhi)")
	fs.StringVar(&h.camera, "headpose-camera", "", "file della calibrazione della camera (default ~/.config/tobii4c/camera.json)")
	fs.BoolVar(&h.always, "headpose-images-always", false, "tiene attivo lo stream immagine anche con l'utente assente (di default si sospende dopo 3 s: il 4C, mentre invia le immagini, salta circa un frame gaze al minuto)")
}

// setup crea lo stimatore secondo -headpose; nil se disattivato.
func (h *headposeFlags) setup(log *slog.Logger) (*headpose.Estimator, error) {
	switch h.mode {
	case "off":
		return nil, nil
	case "auto", "on":
	default:
		return nil, fmt.Errorf("-headpose: %q non valido (auto, on, off)", h.mode)
	}
	if _, err := os.Stat(h.model); err != nil {
		if h.mode == "auto" && errors.Is(err, fs.ErrNotExist) {
			log.Info("head pitch disabled: face model not installed (make headpose-model)", "path", h.model)
			return nil, nil
		}
		return nil, fmt.Errorf("-headpose-model: %w", err)
	}
	cam := h.camera
	if cam == "" {
		p, err := store.CameraPath()
		if err != nil {
			return nil, err
		}
		cam = p
	}
	if h.debug != "" {
		if err := os.MkdirAll(h.debug, 0o755); err != nil {
			return nil, err
		}
	}
	user, err := store.HeadPoseUserPath()
	if err != nil {
		return nil, err
	}
	return headpose.New(headpose.Config{ModelPath: h.model, CameraPath: cam, UserPath: user, Workers: h.workers,
		MaxRate: h.rate, DebugDir: h.debug}, log)
}

// logHeadPoseStats scrive periodicamente lo stato dello stimatore.
func logHeadPoseStats(ctx context.Context, e *headpose.Estimator, every time.Duration, log *slog.Logger) {
	if every <= 0 {
		return
	}
	tk := time.NewTicker(every)
	defer tk.Stop()
	var prev headpose.Stats
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
		}
		st := e.TakeStats()
		frames, done := st.Frames-prev.Frames, st.Processed-prev.Processed
		tracked := st.Tracked - prev.Tracked
		attrs := []any{
			"images", frames, "processed", done, "dropped", st.Dropped - prev.Dropped,
			"face_pct", pct(tracked, done), "searches", st.Searches - prev.Searches,
			"ms_per_image", math.Round(st.MeanProcessMs*10) / 10, "camera_calibrated", st.Calibrated,
		}
		if st.Last.Valid {
			attrs = append(attrs, "pitch_deg", round1(deg(st.Last.Pitch)), "yaw_img_deg", round1(deg(st.Last.Yaw)),
				"roll_img_deg", round1(deg(st.Last.Roll)))
		}
		if st.Compared > 0 {
			// la stessa rotazione vista dall'immagine e dalla linea degli occhi:
			// lo scarto misura la coerenza della calibrazione della camera
			attrs = append(attrs, "yaw_img_minus_eyes_deg", round1(st.YawDiffDeg), "roll_img_minus_eyes_deg", round1(st.RollDiffDeg),
				"yaw_rms_deg", round1(st.YawRMSDeg), "roll_rms_deg", round1(st.RollRMSDeg))
		}
		log.Info("head pose", attrs...)
		prev = st
	}
}

func pct(a, b uint64) float64 {
	if b == 0 {
		return 0
	}
	return math.Round(1000*float64(a)/float64(b)) / 10
}
func deg(r float64) float64    { return r * 180 / math.Pi }
func round1(v float64) float64 { return math.Round(v*10) / 10 }

// cmdFace esegue il modello del viso su un'immagine (PGM di `dump -images`,
// PNG o JPEG): serve a controllare i landmark senza il dispositivo.
func cmdFace(args []string) error {
	fset := flag.NewFlagSet("face", flag.ExitOnError)
	model := fset.String("model", defaultModelPath(), "modello ONNX dei landmark del viso")
	out := fset.String("out", "", "salva qui un PNG annotato")
	workers := fset.Int("workers", 1, "goroutine dell'inferenza")
	runs := fset.Int("bench", 0, "ripete l'inferenza N volte e stampa il tempo medio")
	fset.Usage = func() {
		fmt.Fprintln(os.Stderr, "uso: tobii4c face [flag] IMMAGINE")
		fset.PrintDefaults()
	}
	// l'immagine può stare prima o dopo i flag
	var path string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		path, args = args[0], args[1:]
	}
	fset.Parse(args)
	if path == "" && fset.NArg() == 1 {
		path = fset.Arg(0)
	}
	if path == "" || fset.NArg() > 1 || (fset.NArg() == 1 && path != fset.Arg(0)) {
		fset.Usage()
		return errors.New("serve una sola immagine")
	}
	det, err := headpose.NewDetector(*model, *workers)
	if err != nil {
		return err
	}
	f, err := loadGrayImage(path)
	if err != nil {
		return err
	}
	var lm headpose.Landmarks
	t0 := time.Now()
	crop, score := det.Search(f, &lm)
	fmt.Printf("immagine %dx%d, ricerca %v, probabilità del viso %.3f\n", f.W, f.H, time.Since(t0).Round(time.Millisecond), score)
	if score >= 0.5 {
		fmt.Printf("ritaglio: centro (%.0f, %.0f), lato %.0f px, rotazione %+.1f°\n", crop.CX, crop.CY, crop.Side, deg(crop.Angle))
		for _, p := range []struct {
			name string
			i    int
		}{{"iride destra (468)", 468}, {"iride sinistra (473)", 473}, {"punta del naso (4)", 4}, {"angolo esterno occhio destro (33)", 33}, {"angolo esterno occhio sinistro (263)", 263}} {
			fmt.Printf("  %-38s (%.1f, %.1f)\n", p.name, lm[p.i][0], lm[p.i][1])
		}
		pitch, yaw, roll := headpose.CameraRelativePose(&lm, f.W, f.H)
		fmt.Printf("posa rispetto all'asse della camera (focale ipotetica 700 px): pitch %+.1f°, yaw %+.1f°, roll %+.1f°\n",
			deg(pitch), deg(yaw), deg(roll))
	}
	if *runs > 0 {
		t0 := time.Now()
		for i := 0; i < *runs; i++ {
			det.Detect(f, crop, &lm)
		}
		fmt.Printf("inferenza: %.1f ms in media su %d (%d MMAC)\n", float64(time.Since(t0).Microseconds())/1000/float64(*runs), *runs, det.MACs()/1_000_000)
	}
	if *out != "" {
		var lp *headpose.Landmarks
		if score >= 0.5 {
			lp = &lm
		}
		img := headpose.Annotate(f, lp, crop)
		fh, err := os.Create(*out)
		if err != nil {
			return err
		}
		defer fh.Close()
		if err := png.Encode(fh, img); err != nil {
			return err
		}
		fmt.Println("annotata:", *out)
	}
	return nil
}

func loadGrayImage(path string) (*irimg.Frame, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	if filepath.Ext(path) == ".pgm" {
		return irimg.ReadPGM(fh)
	}
	img, _, err := image.Decode(fh)
	if err != nil {
		return nil, err
	}
	b := img.Bounds()
	f := &irimg.Frame{W: b.Dx(), H: b.Dy(), Pix: make([]byte, b.Dx()*b.Dy())}
	for y := 0; y < f.H; y++ {
		for x := 0; x < f.W; x++ {
			f.Pix[y*f.W+x] = color.GrayModel.Convert(img.At(b.Min.X+x, b.Min.Y+y)).(color.Gray).Y
		}
	}
	return f, nil
}
