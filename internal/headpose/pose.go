package headpose

import (
	"math"

	"github.com/ablescia/tobii4c-linux/internal/ttp"
)

var (
	basisSrc [len(procrustesBasis)]vec3 // modello canonico, mm
	basisW   [len(procrustesBasis)]float64
)

func init() {
	for i, p := range procrustesBasis {
		basisSrc[i] = vec3{p.x * 10, p.y * 10, p.z * 10}
		basisW[i] = p.weight
	}
}

// headInCamera stima la rotazione del modello canonico nel sistema della
// camera, come la pipeline face_geometry di MediaPipe:
//  1. Procrustes in prospettiva debole sui landmark in pixel → scala
//     (pixel per mm) e quindi profondità approssimata del viso;
//  2. retroproiezione di ogni landmark lungo il suo raggio con quella
//     profondità più la z relativa del modello;
//  3. Procrustes metrico tra modello canonico e punti 3D.
//
// Restituisce R (assi canonici → assi della camera) e il centro del viso nel
// sistema della camera (mm).
func headInCamera(lm *Landmarks, cam *Camera) (R mat3, center vec3, ok bool) {
	var dst [len(procrustesBasis)]vec3
	for i, p := range procrustesBasis {
		l := lm[p.id]
		dst[i] = vec3{l[0] - cam.CX, l[1] - cam.CY, l[2]}
	}
	_, s1, _ := procrustes(basisSrc[:], dst[:], basisW[:])
	if !(s1 > 0) {
		return R, center, false
	}
	depth := cam.F / s1 // mm: un millimetro del viso vale s1 pixel
	for it := 0; it < 3; it++ {
		var zmean, ws float64
		for i, p := range procrustesBasis {
			zmean += basisW[i] * lm[p.id][2]
			ws += basisW[i]
		}
		zmean /= ws
		for i, p := range procrustesBasis {
			l := lm[p.id]
			// z del modello: pixel alla profondità del viso → mm
			z := depth + (l[2]-zmean)*depth/cam.F
			dst[i] = vec3{(l[0] - cam.CX) * z / cam.F, (l[1] - cam.CY) * z / cam.F, z}
		}
		var s2 float64
		var t vec3
		R, s2, t = procrustes(basisSrc[:], dst[:], basisW[:])
		if !(s2 > 0) {
			return R, center, false
		}
		center = t
		// s2 ≠ 1: la profondità usata era sbagliata (o il viso è più grande o
		// più piccolo del canonico, e allora cambia solo la distanza stimata)
		depth /= s2
	}
	return R, center, true
}

// displayBasis restituisce la rotazione tracker → schermo della display area
// (righe: assi X, Y, Z dello schermo nel sistema del tracker).
func displayBasis(d ttp.DisplayArea) mat3 {
	tl := vec3{d.TopLeft[0], d.TopLeft[1], d.TopLeft[2]}
	tr := vec3{d.TopRight[0], d.TopRight[1], d.TopRight[2]}
	bl := vec3{d.BottomLeft[0], d.BottomLeft[1], d.BottomLeft[2]}
	x := unit(sub(tr, tl))
	y := unit(sub(tl, bl))
	z := unit(cross(x, y))
	y = cross(z, x) // ortogonalizza
	return mat3{x, y, z}
}

// HeadRotation è la rotazione della testa nel sistema dello schermo.
type HeadRotation struct {
	Pitch, Yaw, Roll float64 // rad, vedi Euler
	R                mat3    // posa neutra → posa attuale
}

// headInDisplay porta la rotazione del modello canonico dalla camera allo
// schermo e la esprime rispetto alla posa neutra.
func headInDisplay(Rcam mat3, cam *Camera, toDisplay mat3) HeadRotation {
	Rtracker := cam.R.t().mul(Rcam)
	Rdisplay := toDisplay.mul(Rtracker)
	R := Rdisplay.mul(headToScreen.t())
	p, y, r := Euler(R)
	return HeadRotation{Pitch: p, Yaw: y, Roll: r, R: R}
}

func deg(r float64) float64 { return r * 180 / math.Pi }

// CameraRelativePose stima la posa della testa rispetto all'asse ottico di
// una camera ipotetica (focale 700 px, punto principale al centro), senza
// calibrazione: solo per controlli a vista (comando face).
func CameraRelativePose(lm *Landmarks, w, h int) (pitch, yaw, roll float64) {
	p, ok := PoseInCamera(lm, w, h, 700)
	if !ok {
		return math.NaN(), math.NaN(), math.NaN()
	}
	return p.Pitch, p.Yaw, p.Roll
}

// FacePose è la posa del viso rispetto a una camera qualsiasi, non legata al
// tracker (es. una webcam).
type FacePose struct {
	Pitch, Yaw, Roll float64 // rad, come Euler con lo "schermo" sulla camera
	// Center è l'origine del modello canonico (dietro la radice del naso)
	// nel sistema della camera, mm: x a destra nell'immagine, y in basso,
	// z lungo l'asse ottico.
	Center [3]float64
}

// PoseInCamera stima la posa del viso rispetto all'asse ottico di una camera
// pinhole con focale focal (pixel) e punto principale al centro
// dell'immagine w×h. Senza calibrazione la focale è un'ipotesi: gli angoli
// ne dipendono poco, la distanza in proporzione.
func PoseInCamera(lm *Landmarks, w, h int, focal float64) (FacePose, bool) {
	cam := &Camera{F: focal, CX: float64(w) / 2, CY: float64(h) / 2, R: ident3, W: w, H: h}
	Rc, center, ok := headInCamera(lm, cam)
	if !ok {
		return FacePose{}, false
	}
	// "schermo" coincidente con la camera, rivolto all'utente: X = −x, Y = −y, Z = +z
	hr := headInDisplay(Rc, cam, mat3{{-1, 0, 0}, {0, -1, 0}, {0, 0, 1}})
	return FacePose{Pitch: hr.Pitch, Yaw: hr.Yaw, Roll: hr.Roll, Center: center}, true
}
