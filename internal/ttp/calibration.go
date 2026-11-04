package ttp

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// Op di calibrazione e geometria. Sequenza catturata dallo Stream Engine
// Windows durante una calibrazione a 7 punti (Krazubu/tobii-linux,
// calibration.pcapng):
//
//	1100 read  → blob attivo (backup)
//	1010 start → notifica 3220 con id 0
//	1060 clear
//	per batch {1, 3, 3} punti:  1030 collect ×n (blocca ~0,6-0,9 s ciascuno)
//	                            1070 compute (~2 s) → notifica 3220 col nuovo id
//	                            1100 read
//	1020 stop
//
// I comandi senza parametri non hanno payload (nemmeno 00 00).
const (
	OpCalStart     uint32 = 0x3f2 // 1010
	OpCalStop      uint32 = 0x3fc // 1020
	OpCalCollect2D uint32 = 0x406 // 1030: 00 00 + fix(x) + fix(y) + u32(maschera occhi)
	OpCalClear     uint32 = 0x424 // 1060
	OpCalCompute   uint32 = 0x42e // 1070: calcola E applica
	OpCalDiscard2D uint32 = 0x438 // 1080: stesso payload del collect
	OpCalRead      uint32 = 0x44c // 1100: risposta = TLV 0x15 col blob
	OpCalWrite     uint32 = 0x456 // 1110: 00 00 + TLV 0x15 col blob
	OpMounting     uint32 = 0x83e // 2110: geometria di montaggio del tracker

	NotifyDisplayArea   uint32 = 0x5aa // 1450: la display area è cambiata
	NotifyCalibrationID uint32 = 0xc94 // 3220: nuova calibrazione attiva (u32 id)
)

// Maschera occhi del collect: 1 sinistro, 2 destro, 3 entrambi (lo Stream
// Engine usa sempre 3). Zero non significa "entrambi": il punto viene scartato.
const EyesBoth uint32 = 3

// CalibrationPoints7 è lo schema a 7 punti dell'applicazione Windows, nell'ordine
// catturato, con i batch dopo cui viene eseguito un compute.
var CalibrationPoints7 = [7][2]float32{
	{0.5, 0.5},
	{0.5, 0.1}, {0.9, 0.9}, {0.1, 0.9},
	{0.1, 0.1}, {0.9, 0.1}, {0.5, 0.9},
}

// CalibrationBatches7 indicizza CalibrationPoints7.
var CalibrationBatches7 = [][]int{{0}, {1, 2, 3}, {4, 5, 6}}

// fixed3232 riproduce l'arrotondamento dello Stream Engine: x*1024 calcolato
// in float32, poi portato a 32.32 (= x * 2^42).
func fixed3232(v float32) uint64 {
	return uint64(int64(math.Round(float64(v*1024) * (1 << 32))))
}

// CalPointPayload costruisce il payload di collect (1030) e discard (1080).
func CalPointPayload(x, y float32, eyeMask uint32) []byte {
	p := PayloadPrefix(make([]byte, 0, 2+13+13+9))
	p = append(p, TypeFixed42, 0, 0, 0, 8)
	p = binary.BigEndian.AppendUint64(p, fixed3232(x))
	p = append(p, TypeFixed42, 0, 0, 0, 8)
	p = binary.BigEndian.AppendUint64(p, fixed3232(y))
	return AppendU32(p, eyeMask)
}

// CalWritePayload: 00 00 + 15 [n+4] [n] blob.
func CalWritePayload(blob []byte) []byte {
	p := PayloadPrefix(make([]byte, 0, 2+9+len(blob)))
	p = append(p, TypeBytes)
	p = binary.BigEndian.AppendUint32(p, uint32(len(blob)+4))
	p = binary.BigEndian.AppendUint32(p, uint32(len(blob)))
	return append(p, blob...)
}

// CalBlobFromPayload estrae il blob da una risposta 1100 (o da un payload 1110).
func CalBlobFromPayload(payload []byte) ([]byte, error) {
	r := NewPayloadReader(payload)
	t, ok := r.Next()
	if !ok || t.Type != TypeBytes {
		return nil, errors.New("calibration: response is not a byte blob")
	}
	b, ok := t.Counted()
	if !ok {
		return nil, errors.New("calibration: truncated blob")
	}
	return b, nil
}

// Mounting è la geometria di montaggio dichiarata dal tracker (op 2110), il
// tobii_geometry_mounting_t dello Stream Engine.
type Mounting struct {
	Guides     int        `json:"guides"`      // segni guida sul corpo del tracker
	WidthMM    float64    `json:"width_mm"`    // distanza tra i segni guida
	AngleDeg   float64    `json:"angle_deg"`   // inclinazione dell'asse camera rispetto allo schermo
	ExternalMM [3]float64 `json:"external_mm"` // offset del punto di montaggio visto da fuori
	InternalMM [3]float64 `json:"internal_mm"` // offset dell'origine camera dentro il tracker
}

// ParseMounting decodifica la risposta di 2110:
// 00 00 + u32 guides + fix16 width + fix16 angle + point3d external + point3d internal.
func ParseMounting(payload []byte) (Mounting, error) {
	r := NewPayloadReader(payload)
	var vals [5]Value
	for i := range vals {
		if err := readValue(&r, &vals[i]); err != nil {
			return Mounting{}, fmt.Errorf("mounting field %d: %w", i, err)
		}
	}
	if vals[0].Kind != KindU32 || vals[3].Kind != KindPoint || vals[4].Kind != KindPoint {
		return Mounting{}, errors.New("mounting: unexpected layout")
	}
	scalar := func(v Value) float64 { return v.F[0] }
	return Mounting{
		Guides:     int(vals[0].U),
		WidthMM:    scalar(vals[1]),
		AngleDeg:   scalar(vals[2]),
		ExternalMM: vals[3].F,
		InternalMM: vals[4].F,
	}, nil
}

// DisplayAreaBasic replica tobii_calculate_display_area_basic dello Stream
// Engine: uno schermo largo w e alto h mm, il cui centro sta offsetX mm a
// destra del centro del tracker. Il bordo inferiore è a internal − Rx(angle)·external
// e lo schermo sale lungo l'asse y inclinato della camera. Riproduce le display
// area delle catture Windows a meno di un micrometro (Krazubu/tobii-linux, geometry.rs).
func DisplayAreaBasic(w, h, offsetX float64, m Mounting) DisplayArea {
	s, c := math.Sincos(m.AngleDeg * math.Pi / 180)
	e := m.ExternalMM
	rot := [3]float64{e[0], c*e[1] - s*e[2], s*e[1] + c*e[2]}
	var bottomCentre [3]float64
	for i := range bottomCentre {
		bottomCentre[i] = m.InternalMM[i] - rot[i]
	}
	up := [3]float64{0, c, s}
	bl := bottomCentre
	bl[0] += offsetX - w/2
	tl := [3]float64{bl[0] + up[0]*h, bl[1] + up[1]*h, bl[2] + up[2]*h}
	tr := tl
	tr[0] += w
	return DisplayArea{TopLeft: tl, TopRight: tr, BottomLeft: bl}
}

// OffsetX restituisce di quanto il centro della display area sta a destra del
// centro del tracker (inverso di DisplayAreaBasic per la componente x).
func (d DisplayArea) OffsetX() float64 { return (d.TopLeft[0] + d.TopRight[0]) / 2 }

// ToDisplay esprime un punto del sistema del tracker (mm) nel sistema dello
// schermo dello Stream Engine (gaze_origin, head_pose): origine al centro
// della display area, +X lungo il bordo superiore verso destra, +Y lungo il
// bordo sinistro verso l'alto, +Z = X × Y verso l'utente. È la trasformazione
// che il firmware applica alle colonne "Gaze Origin Display" (0x22/0x24):
// su una cattura ET5 le riproduce a meno di 2 µm.
func (d DisplayArea) ToDisplay(p [3]float64) [3]float64 {
	var c, ex, ey [3]float64
	for i := range c {
		c[i] = (d.TopRight[i] + d.BottomLeft[i]) / 2
		ex[i] = d.TopRight[i] - d.TopLeft[i]
		ey[i] = d.TopLeft[i] - d.BottomLeft[i]
	}
	ex, ey = unit(ex), unit(ey)
	ez := [3]float64{ex[1]*ey[2] - ex[2]*ey[1], ex[2]*ey[0] - ex[0]*ey[2], ex[0]*ey[1] - ex[1]*ey[0]}
	r := [3]float64{p[0] - c[0], p[1] - c[1], p[2] - c[2]}
	dot := func(a [3]float64) float64 { return a[0]*r[0] + a[1]*r[1] + a[2]*r[2] }
	return [3]float64{dot(ex), dot(ey), dot(ez)}
}

func unit(v [3]float64) [3]float64 {
	n := math.Sqrt(v[0]*v[0] + v[1]*v[1] + v[2]*v[2])
	if n == 0 {
		return v
	}
	return [3]float64{v[0] / n, v[1] / n, v[2] / n}
}

// PointMM converte coordinate normalizzate del display (0,0 in alto a
// sinistra) in un punto 3D nel sistema del tracker, mm.
func (d DisplayArea) PointMM(u, v float64) [3]float64 {
	var p [3]float64
	for i := range p {
		p[i] = d.TopLeft[i] + u*(d.TopRight[i]-d.TopLeft[i]) + v*(d.BottomLeft[i]-d.TopLeft[i])
	}
	return p
}
