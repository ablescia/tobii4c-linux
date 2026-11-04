package headpose

import (
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/ablescia/tobii4c-linux/internal/irimg"
)

// debugImage salva, al più ogni DebugEvery, l'immagine con i landmark (verde),
// i punti chiave (rosso), le iridi (azzurro), il ritaglio (giallo) e gli occhi
// del tracker proiettati con la camera calibrata (magenta).
func (e *Estimator) debugImage(f *irimg.Frame, lm *Landmarks, c Crop, cam *Camera, l, r vec3, eyesOK bool, res Result) {
	if e.cfg.DebugDir == "" || time.Since(e.lastDebug) < e.cfg.DebugEvery {
		return
	}
	e.lastDebug = time.Now()
	img := Annotate(f, lm, c)
	if cam != nil && eyesOK {
		for _, p := range []vec3{l, r} {
			if u, v, ok := cam.Project(p); ok {
				ring(img, u, v, 5, color.RGBA{255, 0, 255, 255})
			}
		}
	}
	name := fmt.Sprintf("hp-%d-score%03.0f", f.DeviceUs, res.Score*100)
	if res.Valid {
		name += fmt.Sprintf("-pitch%+.1f-yaw%+.1f-roll%+.1f", deg(res.Pitch), deg(res.Yaw), deg(res.Roll))
	}
	path := filepath.Join(e.cfg.DebugDir, name+".png")
	fh, err := os.Create(path)
	if err != nil {
		e.log.Warn("head pose: debug image", "err", err)
		return
	}
	defer fh.Close()
	if err := png.Encode(fh, img); err != nil {
		e.log.Warn("head pose: debug image", "err", err)
	}
}

// Annotate disegna landmark e ritaglio su una copia a colori del frame.
func Annotate(f *irimg.Frame, lm *Landmarks, c Crop) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, f.W, f.H))
	for i, v := range f.Pix[:f.W*f.H] {
		img.Pix[4*i], img.Pix[4*i+1], img.Pix[4*i+2], img.Pix[4*i+3] = v, v, v, 255
	}
	if c.Side > 0 {
		for k := 0; k < 4; k++ {
			for s := 0.0; s < 1; s += 0.002 {
				u, v := [4]float64{s, 1, 1 - s, 0}[k]*ModelInput, [4]float64{0, s, 1, 1 - s}[k]*ModelInput
				x, y := c.ToImage(u, v, ModelInput)
				img.Set(int(x), int(y), color.RGBA{255, 220, 0, 255})
			}
		}
	}
	if lm == nil {
		return img
	}
	for i := 0; i < 468; i++ {
		img.Set(int(lm[i][0]), int(lm[i][1]), color.RGBA{0, 230, 0, 255})
	}
	for _, i := range []int{lmRightEyeOuter, lmRightEyeInner, lmLeftEyeOuter, lmLeftEyeInner, lmNoseTip} {
		plotDot(img, lm[i][0], lm[i][1], 1, color.RGBA{255, 40, 40, 255})
	}
	for _, i := range []int{lmRightIris, lmLeftIris} {
		plotDot(img, lm[i][0], lm[i][1], 1, color.RGBA{0, 160, 255, 255})
	}
	return img
}

func plotDot(img *image.RGBA, x, y float64, r int, c color.RGBA) {
	for dy := -r; dy <= r; dy++ {
		for dx := -r; dx <= r; dx++ {
			img.Set(int(x)+dx, int(y)+dy, c)
		}
	}
}

func ring(img *image.RGBA, x, y, r float64, c color.RGBA) {
	for a := 0.0; a < 6.3; a += 0.05 {
		img.Set(int(x+r*math.Cos(a)), int(y+r*math.Sin(a)), c)
	}
}
