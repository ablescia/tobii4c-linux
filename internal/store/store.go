// Package store gestisce i file persistenti in ~/.config/tobii4c:
//
//	calibration.bin           ultima calibrazione salvata (blob opaco del dispositivo)
//	display.json              ultima geometria dello schermo applicata
//	camera.json               calibrazione della camera per il pitch della testa (internal/headpose)
//	headpose-user.json        zero personale del pitch
//	backups/calibration-*.bin calibrazione presente sul dispositivo prima di ogni ricalibrazione
package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/ablescia/tobii4c-linux/internal/ttp"
)

// Dir restituisce (e crea) la cartella di configurazione.
func Dir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	d := filepath.Join(base, "tobii4c")
	if err := os.MkdirAll(filepath.Join(d, "backups"), 0o755); err != nil {
		return "", err
	}
	return d, nil
}

// CalibrationPath è il file della calibrazione salvata.
func CalibrationPath() (string, error) {
	d, err := Dir()
	return filepath.Join(d, "calibration.bin"), err
}

// DisplayPath è il file della geometria dello schermo.
func DisplayPath() (string, error) {
	d, err := Dir()
	return filepath.Join(d, "display.json"), err
}

// HeadPoseUserPath è il file con lo zero personale del pitch della testa.
func HeadPoseUserPath() (string, error) {
	d, err := Dir()
	return filepath.Join(d, "headpose-user.json"), err
}

// CameraPath è il file della calibrazione della camera (pitch della testa).
func CameraPath() (string, error) {
	d, err := Dir()
	return filepath.Join(d, "camera.json"), err
}

// WriteAtomic scrive via file temporaneo + rename: un crash non lascia mai un
// blob troncato al posto di uno buono.
func WriteAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	tmp.Close()
	return os.Rename(tmp.Name(), path)
}

// SaveBackup salva un blob nella cartella backups con timestamp.
func SaveBackup(blob []byte, tag string) (string, error) {
	d, err := Dir()
	if err != nil {
		return "", err
	}
	p := filepath.Join(d, "backups", fmt.Sprintf("calibration-%s-%s.bin", time.Now().Format("20060102-150405"), tag))
	return p, WriteAtomic(p, blob)
}

// Backups elenca i backup, dal più recente.
func Backups() ([]string, error) {
	d, err := Dir()
	if err != nil {
		return nil, err
	}
	m, err := filepath.Glob(filepath.Join(d, "backups", "calibration-*.bin"))
	sort.Sort(sort.Reverse(sort.StringSlice(m)))
	return m, err
}

// Display è la geometria salvata.
type Display struct {
	WidthMM   float64          `json:"width_mm"`
	HeightMM  float64          `json:"height_mm"`
	OffsetXMM float64          `json:"offset_x_mm"`
	Monitor   string           `json:"monitor,omitempty"`
	Area      *ttp.DisplayArea `json:"area,omitempty"`
	SavedAt   time.Time        `json:"saved_at"`
}

// SaveDisplay salva la geometria.
func SaveDisplay(d Display) error {
	p, err := DisplayPath()
	if err != nil {
		return err
	}
	d.SavedAt = time.Now()
	b, _ := json.MarshalIndent(d, "", "  ")
	return WriteAtomic(p, b)
}

// LoadDisplay legge la geometria salvata.
func LoadDisplay() (Display, error) {
	var d Display
	p, err := DisplayPath()
	if err != nil {
		return d, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return d, err
	}
	return d, json.Unmarshal(b, &d)
}
