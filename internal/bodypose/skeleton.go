package bodypose

// Indici dei 33 punti di MediaPipe Pose. Il modello del mezzo busto ha i
// primi 25 (fino ai fianchi) con gli stessi indici. "Sinistro" e "destro"
// sono quelli del soggetto.
const (
	Nose = iota
	LeftEyeInner
	LeftEye
	LeftEyeOuter
	RightEyeInner
	RightEye
	RightEyeOuter
	LeftEar
	RightEar
	MouthLeft
	MouthRight
	LeftShoulder
	RightShoulder
	LeftElbow
	RightElbow
	LeftWrist
	RightWrist
	LeftPinky
	RightPinky
	LeftIndex
	RightIndex
	LeftThumb
	RightThumb
	LeftHip
	RightHip
	LeftKnee
	RightKnee
	LeftAnkle
	RightAnkle
	LeftHeel
	RightHeel
	LeftFootIndex
	RightFootIndex

	NumLandmarks      = 33
	NumUpperLandmarks = 25
)

// Names sono i nomi italiani dei punti.
var Names = [NumLandmarks]string{
	"naso", "occhio sx interno", "occhio sx", "occhio sx esterno", "occhio dx interno", "occhio dx",
	"occhio dx esterno", "orecchio sx", "orecchio dx", "bocca sx", "bocca dx", "spalla sx", "spalla dx",
	"gomito sx", "gomito dx", "polso sx", "polso dx", "mignolo sx", "mignolo dx", "indice sx", "indice dx",
	"pollice sx", "pollice dx", "anca sx", "anca dx", "ginocchio sx", "ginocchio dx", "caviglia sx",
	"caviglia dx", "tallone sx", "tallone dx", "punta del piede sx", "punta del piede dx",
}

// Side dice da che parte del soggetto sta un punto: -1 sinistra, 1 destra,
// 0 al centro (il naso).
func Side(i int) int {
	switch i {
	case Nose:
		return 0
	case LeftEyeInner, LeftEye, LeftEyeOuter, LeftEar, MouthLeft:
		return -1
	case RightEyeInner, RightEye, RightEyeOuter, RightEar, MouthRight:
		return 1
	}
	if i >= LeftShoulder && i%2 == 1 {
		return -1
	}
	return 1
}

// Bone è un segmento dello scheletro.
type Bone [2]int

// Bones sono i segmenti di POSE_CONNECTIONS di MediaPipe.
var Bones = []Bone{
	{0, 1}, {1, 2}, {2, 3}, {3, 7}, {0, 4}, {4, 5}, {5, 6}, {6, 8}, {9, 10},
	{11, 12}, {11, 13}, {13, 15}, {15, 17}, {15, 19}, {15, 21}, {17, 19},
	{12, 14}, {14, 16}, {16, 18}, {16, 20}, {16, 22}, {18, 20},
	{11, 23}, {12, 24}, {23, 24}, {23, 25}, {24, 26}, {25, 27}, {26, 28},
	{27, 29}, {28, 30}, {29, 31}, {30, 32}, {27, 31}, {28, 32},
}

// Part classifica i segmenti per il disegno.
func (b Bone) Part() int {
	a, c := b[0], b[1]
	switch {
	case a <= RightEar && c <= RightEar, a == MouthLeft:
		return PartFace
	case a == LeftShoulder && c == RightShoulder, a == LeftHip && c == RightHip,
		a == LeftShoulder && c == LeftHip, a == RightShoulder && c == RightHip:
		return PartTorso
	}
	return Side(c)
}

// Parti dello scheletro (oltre a -1 e 1 per i lati).
const (
	PartFace  = 2
	PartTorso = 3
)
