package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/ablescia/tobii4c-linux/internal/headpose"
	"github.com/ablescia/tobii4c-linux/internal/store"
)

// headposeFlags sono i flag del pitch della testa (comando run).
type headposeFlags struct {
	mode     string
	workers  int
	rate     float64
	camera   string
	always   bool
	rotation string
}

func (h *headposeFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&h.mode, "headpose", "on", "pitch della testa dalle immagini IR: on, off")
	fs.IntVar(&h.workers, "headpose-workers", 1, "goroutine dell'inferenza del modello del viso (più goroutine: meno latenza, più CPU)")
	fs.Float64Var(&h.rate, "headpose-rate", 30, "inferenze al secondo al massimo (le immagini arrivano a 30 Hz)")
	fs.StringVar(&h.camera, "headpose-camera", "", "file della calibrazione della camera (default ~/.config/tobii4c/camera.json)")
	fs.StringVar(&h.rotation, "headpose-rotation", "image", "da dove vengono yaw e roll della testa: image (dal viso quando è tracciato, dagli occhi altrimenti; sul 4C lo yaw degli occhi è sottostimato di circa 5 volte), eyes (sempre dagli occhi)")
	fs.BoolVar(&h.always, "headpose-images-always", false, "tiene attivo lo stream immagine anche con l'utente assente (di default si sospende dopo 3 s: il 4C, mentre invia le immagini, salta circa un frame gaze al minuto)")
}

// setup crea lo stimatore secondo -headpose; nil se disattivato.
func (h *headposeFlags) setup(log *slog.Logger) (*headpose.Estimator, error) {
	switch h.mode {
	case "off":
		return nil, nil
	case "on", "auto": // auto: vecchio default, quando il modello andava installato a parte
	default:
		return nil, fmt.Errorf("-headpose: %q non valido (on, off)", h.mode)
	}
	if h.rotation != "image" && h.rotation != "eyes" {
		return nil, fmt.Errorf("-headpose-rotation: %q non valido (image, eyes)", h.rotation)
	}
	cam := h.camera
	if cam == "" {
		p, err := store.CameraPath()
		if err != nil {
			return nil, err
		}
		cam = p
	}
	user, err := store.HeadPoseUserPath()
	if err != nil {
		return nil, err
	}
	return headpose.New(headpose.Config{CameraPath: cam, UserPath: user, Workers: h.workers,
		MaxRate: h.rate, ImageRotation: h.rotation == "image"}, log)
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
