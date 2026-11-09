// Package bodypose rileva lo scheletro di una persona con i modelli di
// Google MediaPipe Pose, eseguiti da internal/tflite sulla GPU (Vulkan) o
// sulla CPU:
//
//   - corpo intero (FullBody): MediaPipe Pose Landmarker, il modello attuale
//     (BlazePose GHUM 3D, varianti lite, full e heavy): 33 punti, in piedi o
//     seduti, con le coordinate 3D in metri e la maschera della persona;
//   - mezzo busto (UpperBody): il modello "upper body" di MediaPipe Pose
//     0.8.3, addestrato sulla sola parte superiore del corpo (persona seduta
//     alla scrivania): 25 punti, la maschera, nessuna coordinata 3D.
//
// La pipeline replica quella di MediaPipe: detector SSD con ancore e NMS
// pesata sul frame intero, poi la rete dei landmark su un ritaglio ruotato;
// nei frame successivi il ritaglio viene dai punti ausiliari del frame
// precedente (inseguimento) e il detector torna in gioco solo quando la
// persona si perde. Filtri One Euro sui punti come in MediaPipe.
package bodypose

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/ablescia/tobii4c-linux/internal/tflite"
)

// Mode è la tipologia di rilevamento.
type Mode int

const (
	FullBody  Mode = iota // corpo intero, in piedi o seduto
	UpperBody             // mezzo busto, da seduto
)

func (m Mode) String() string {
	if m == UpperBody {
		return "mezzo busto"
	}
	return "corpo intero"
}

// Varianti del modello del corpo intero (complessità crescente).
const (
	Lite  = "lite"
	Full  = "full"
	Heavy = "heavy"
)

// Variants sono le varianti del corpo intero, dalla più leggera.
var Variants = []string{Lite, Full, Heavy}

// file è un file da scaricare, con l'impronta attesa.
type file struct {
	Name   string // nella cache
	URL    string
	SHA256 string
	Size   int64
}

// Spec descrive i modelli di una modalità.
type Spec struct {
	Mode    Mode
	Variant string
	Title   string
	Source  file
	// membri dell'archivio .task (vuoti: Source è il detector e Extra i
	// landmark)
	DetectorMember, LandmarksMember string
	Extra                           *file
	License                         string
}

const mpLicense = "Apache 2.0, © Google LLC"

var specs = map[string]Spec{
	"full-lite": {Mode: FullBody, Variant: Lite, Title: "MediaPipe Pose Landmarker lite (float16, v1)",
		Source: file{"pose_landmarker_lite.task",
			"https://storage.googleapis.com/mediapipe-models/pose_landmarker/pose_landmarker_lite/float16/1/pose_landmarker_lite.task",
			"59929e1d1ee95287735ddd833b19cf4ac46d29bc7afddbbf6753c459690d574a", 5777746},
		DetectorMember: "pose_detector.tflite", LandmarksMember: "pose_landmarks_detector.tflite", License: mpLicense},
	"full-full": {Mode: FullBody, Variant: Full, Title: "MediaPipe Pose Landmarker full (float16, v1)",
		Source: file{"pose_landmarker_full.task",
			"https://storage.googleapis.com/mediapipe-models/pose_landmarker/pose_landmarker_full/float16/1/pose_landmarker_full.task",
			"5134a3aad27a58b93da0088d431f366da362b44e3ccfbe3462b3827a839011b1", 9398198},
		DetectorMember: "pose_detector.tflite", LandmarksMember: "pose_landmarks_detector.tflite", License: mpLicense},
	"full-heavy": {Mode: FullBody, Variant: Heavy, Title: "MediaPipe Pose Landmarker heavy (float16, v1)",
		Source: file{"pose_landmarker_heavy.task",
			"https://storage.googleapis.com/mediapipe-models/pose_landmarker/pose_landmarker_heavy/float16/1/pose_landmarker_heavy.task",
			"64437af838a65d18e5ba7a0d39b465540069bc8aae8308de3e318aad31fcbc7b", 30664242},
		DetectorMember: "pose_detector.tflite", LandmarksMember: "pose_landmarks_detector.tflite", License: mpLicense},
	"upper": {Mode: UpperBody, Title: "MediaPipe Pose 0.8.3 upper body (BlazePose, 2020)",
		Source: file{"mediapipe-0.8.3.2-pose_detection.tflite",
			"https://raw.githubusercontent.com/google-ai-edge/mediapipe/0.8.3.2/mediapipe/modules/pose_detection/pose_detection.tflite",
			"82b0aec8511eaca13d1ff174aaddf8f5764f117bbb4060995c0d8bf01bbfca91", 3080336},
		Extra: &file{"mediapipe-0.8.3.2-pose_landmark_upper_body.tflite",
			"https://raw.githubusercontent.com/google-ai-edge/mediapipe/0.8.3.2/mediapipe/modules/pose_landmark/pose_landmark_upper_body.tflite",
			"2a0a0af870324f35b1f1bd57594d584321520680f7c8ed19cd1e9cb978476cef", 6914704},
		License: mpLicense},
}

// SpecFor è la descrizione dei modelli di una modalità (variant vale solo
// per il corpo intero; vuota: full).
func SpecFor(m Mode, variant string) (Spec, error) {
	key := "upper"
	if m == FullBody {
		if variant == "" {
			variant = Full
		}
		key = "full-" + variant
	}
	s, ok := specs[key]
	if !ok {
		return Spec{}, fmt.Errorf("bodypose: variante %q sconosciuta (lite, full, heavy)", variant)
	}
	return s, nil
}

// DefaultDir è la cartella della cache dei modelli: $XDG_CACHE_HOME o
// ~/.cache, sotto tobii4c/models.
func DefaultDir() string {
	d, err := os.UserCacheDir()
	if err != nil {
		d = os.TempDir()
	}
	return filepath.Join(d, "tobii4c", "models")
}

// Models sono le due reti di una modalità, già lette.
type Models struct {
	Spec      Spec
	Detector  *tflite.Model
	Landmarks *tflite.Model
	Files     []string // percorsi nella cache
}

// Progress riceve i messaggi dello scaricamento (può essere nil).
type Progress func(msg string)

// Load legge i modelli di una modalità dalla cartella dir (vuota:
// DefaultDir), scaricandoli dai server di Google se mancano. Ogni file è
// verificato con lo SHA-256 atteso.
func Load(m Mode, variant, dir string, offline bool, progress Progress) (*Models, error) {
	spec, err := SpecFor(m, variant)
	if err != nil {
		return nil, err
	}
	if dir == "" {
		dir = DefaultDir()
	}
	src, err := fetch(spec.Source, dir, offline, progress)
	if err != nil {
		return nil, err
	}
	out := &Models{Spec: spec, Files: []string{filepath.Join(dir, spec.Source.Name)}}
	var det, lm []byte
	if spec.Extra == nil {
		zr, err := zip.NewReader(bytes.NewReader(src), int64(len(src)))
		if err != nil {
			return nil, fmt.Errorf("bodypose: %s: %w", spec.Source.Name, err)
		}
		if det, err = member(zr, spec.DetectorMember); err != nil {
			return nil, err
		}
		if lm, err = member(zr, spec.LandmarksMember); err != nil {
			return nil, err
		}
	} else {
		det = src
		if lm, err = fetch(*spec.Extra, dir, offline, progress); err != nil {
			return nil, err
		}
		out.Files = append(out.Files, filepath.Join(dir, spec.Extra.Name))
	}
	if out.Detector, err = tflite.Parse(det); err != nil {
		return nil, fmt.Errorf("bodypose: detector: %w", err)
	}
	if out.Landmarks, err = tflite.Parse(lm); err != nil {
		return nil, fmt.Errorf("bodypose: landmark: %w", err)
	}
	return out, nil
}

// Ensure si assicura che i file dei modelli di una modalità siano nella
// cache (scaricandoli se serve e se non offline), senza leggere le reti.
func Ensure(m Mode, variant, dir string, offline bool, progress Progress) error {
	spec, err := SpecFor(m, variant)
	if err != nil {
		return err
	}
	if dir == "" {
		dir = DefaultDir()
	}
	for _, f := range []*file{&spec.Source, spec.Extra} {
		if f == nil {
			continue
		}
		if _, err := fetch(*f, dir, offline, progress); err != nil {
			return err
		}
	}
	return nil
}

func member(zr *zip.Reader, name string) ([]byte, error) {
	f, err := zr.Open(name)
	if err != nil {
		return nil, fmt.Errorf("bodypose: %s non è nell'archivio: %w", name, err)
	}
	defer f.Close()
	return io.ReadAll(f)
}

// ErrOffline: il modello non è nella cache e lo scaricamento è disattivato.
var ErrOffline = errors.New("bodypose: modello non presente nella cache")

// fetch restituisce il contenuto di f: dalla cache se c'è e l'impronta è
// giusta, altrimenti scaricandolo.
func fetch(f file, dir string, offline bool, progress Progress) ([]byte, error) {
	path := filepath.Join(dir, f.Name)
	if b, err := os.ReadFile(path); err == nil {
		if sum(b) == f.SHA256 {
			return b, nil
		}
		if progress != nil {
			progress(f.Name + ": impronta diversa da quella attesa, lo riscarico")
		}
	}
	if offline {
		return nil, fmt.Errorf("%w: %s (cartella %s)", ErrOffline, f.Name, dir)
	}
	if progress != nil {
		progress(fmt.Sprintf("scarico %s (%.1f MB) da %s", f.Name, float64(f.Size)/1e6, f.URL))
	}
	b, err := download(f.URL, f.Size)
	if err != nil {
		return nil, fmt.Errorf("bodypose: %s: %w", f.Name, err)
	}
	if got := sum(b); got != f.SHA256 {
		return nil, fmt.Errorf("bodypose: %s: SHA-256 %s, atteso %s", f.Name, got, f.SHA256)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	tmp := path + ".part"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return nil, err
	}
	return b, nil
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func download(url string, size int64) ([]byte, error) {
	cl := &http.Client{Timeout: 5 * time.Minute}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "tobii4c-linux")
	resp, err := cl.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %s", resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 2*size+1<<20))
}
