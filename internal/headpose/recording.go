package headpose

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// Lettura delle registrazioni di headpose-calibrate (frames.jsonl e
// gaze.jsonl), per ricalcolare la calibrazione della camera offline.

type recFrameJSON struct {
	Us    int64        `json:"us"`
	Score float64      `json:"score"`
	LM    [][3]float64 `json:"lm"`
	Ids   []int        `json:"landmark_ids"`
}

type recGazeJSON struct {
	Us int64      `json:"us"`
	L  [3]float64 `json:"l"`
	R  [3]float64 `json:"r"`
	LV bool       `json:"lv"`
	RV bool       `json:"rv"`
}

// imageGazeLagUs: il timestamp di un'immagine 0x501 è in ritardo rispetto a
// quello dei campioni gaze che vedono gli stessi occhi. Misurato sul 4C
// (sessione del 2026-11-04, 7850 coppie): errore minimo tra 16 e 33 ms.
const imageGazeLagUs = 25_000

// RecordingSamples ricostruisce i campioni di calibrazione da una o più
// registrazioni.
func RecordingSamples(dirs ...string) ([]CalibSample, error) {
	var out []CalibSample
	for _, dir := range dirs {
		s, err := recordingSamples(dir)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", dir, err)
		}
		out = append(out, s...)
	}
	if len(out) == 0 {
		return nil, errors.New("no usable samples in the recordings")
	}
	return out, nil
}

func recordingSamples(dir string) ([]CalibSample, error) {
	ff, err := os.Open(filepath.Join(dir, "frames.jsonl"))
	if err != nil {
		return nil, err
	}
	defer ff.Close()
	gf, err := os.Open(filepath.Join(dir, "gaze.jsonl"))
	if err != nil {
		return nil, err
	}
	defer gf.Close()
	var gz []recGazeJSON
	sc := bufio.NewScanner(gf)
	for sc.Scan() {
		var g recGazeJSON
		if json.Unmarshal(sc.Bytes(), &g) == nil {
			gz = append(gz, g)
		}
	}
	sort.Slice(gz, func(i, j int) bool { return gz[i].Us < gz[j].Us })
	var ids []int
	var out []CalibSample
	sc = bufio.NewScanner(ff)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var fr recFrameJSON
		if json.Unmarshal(sc.Bytes(), &fr) != nil {
			continue
		}
		if fr.Ids != nil {
			ids = fr.Ids
			continue
		}
		if fr.Score < 0.9 || len(fr.LM) != len(ids) {
			continue
		}
		us := fr.Us - imageGazeLagUs
		i := sort.Search(len(gz), func(i int) bool { return gz[i].Us >= us })
		if i == 0 || i >= len(gz) {
			continue
		}
		a, b := gz[i-1], gz[i]
		if b.Us-a.Us > 30_000 || !a.LV || !a.RV || !b.LV || !b.RV {
			continue
		}
		t := float64(us-a.Us) / float64(b.Us-a.Us)
		var l, r vec3
		for k := 0; k < 3; k++ {
			l[k] = a.L[k]*(1-t) + b.L[k]*t
			r[k] = a.R[k]*(1-t) + b.R[k]*t
		}
		s := CalibSample{Left: l, Right: r}
		for k, id := range ids {
			switch id {
			case lmRightIris:
				s.Iris468 = [2]float64{fr.LM[k][0], fr.LM[k][1]}
			case lmLeftIris:
				s.Iris473 = [2]float64{fr.LM[k][0], fr.LM[k][1]}
			}
		}
		out = append(out, s)
	}
	return out, sc.Err()
}

// CalibrateSamples stima la camera da campioni già raccolti (tutti).
func CalibrateSamples(samples []CalibSample, w, h int) (*Camera, CalibQuality, error) {
	return calibrateFast(samples, w, h)
}
