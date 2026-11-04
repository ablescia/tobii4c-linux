package headpose

import "math"

type vec3 = [3]float64

// mat3 è una matrice 3×3 per righe.
type mat3 [3][3]float64

var ident3 = mat3{{1, 0, 0}, {0, 1, 0}, {0, 0, 1}}

func (a mat3) mul(b mat3) mat3 {
	var c mat3
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			c[i][j] = a[i][0]*b[0][j] + a[i][1]*b[1][j] + a[i][2]*b[2][j]
		}
	}
	return c
}

func (a mat3) apply(v vec3) vec3 {
	return vec3{
		a[0][0]*v[0] + a[0][1]*v[1] + a[0][2]*v[2],
		a[1][0]*v[0] + a[1][1]*v[1] + a[1][2]*v[2],
		a[2][0]*v[0] + a[2][1]*v[1] + a[2][2]*v[2],
	}
}

func (a mat3) t() mat3 {
	var c mat3
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			c[i][j] = a[j][i]
		}
	}
	return c
}

func sub(a, b vec3) vec3           { return vec3{a[0] - b[0], a[1] - b[1], a[2] - b[2]} }
func add(a, b vec3) vec3           { return vec3{a[0] + b[0], a[1] + b[1], a[2] + b[2]} }
func scale(a vec3, s float64) vec3 { return vec3{a[0] * s, a[1] * s, a[2] * s} }
func dot(a, b vec3) float64        { return a[0]*b[0] + a[1]*b[1] + a[2]*b[2] }
func norm(a vec3) float64          { return math.Sqrt(dot(a, a)) }
func cross(a, b vec3) vec3 {
	return vec3{a[1]*b[2] - a[2]*b[1], a[2]*b[0] - a[0]*b[2], a[0]*b[1] - a[1]*b[0]}
}
func unit(a vec3) vec3 {
	if n := norm(a); n > 0 {
		return scale(a, 1/n)
	}
	return a
}

// rodrigues converte un vettore di rotazione (asse × angolo) in matrice.
func rodrigues(r vec3) mat3 {
	th := norm(r)
	if th < 1e-12 {
		return mat3{{1, -r[2], r[1]}, {r[2], 1, -r[0]}, {-r[1], r[0], 1}}
	}
	k := scale(r, 1/th)
	s, c := math.Sincos(th)
	v := 1 - c
	return mat3{
		{c + k[0]*k[0]*v, k[0]*k[1]*v - k[2]*s, k[0]*k[2]*v + k[1]*s},
		{k[1]*k[0]*v + k[2]*s, c + k[1]*k[1]*v, k[1]*k[2]*v - k[0]*s},
		{k[2]*k[0]*v - k[1]*s, k[2]*k[1]*v + k[0]*s, c + k[2]*k[2]*v},
	}
}

// rotvec è l'inverso di rodrigues.
func rotvec(R mat3) vec3 {
	c := (R[0][0] + R[1][1] + R[2][2] - 1) / 2
	c = math.Max(-1, math.Min(1, c))
	th := math.Acos(c)
	if th < 1e-9 {
		return vec3{}
	}
	if math.Pi-th < 1e-6 {
		// asse dall'autovettore di (R+I)/2
		x := math.Sqrt(math.Max(0, (R[0][0]+1)/2))
		y := math.Sqrt(math.Max(0, (R[1][1]+1)/2))
		z := math.Sqrt(math.Max(0, (R[2][2]+1)/2))
		if x >= y && x >= z {
			y, z = math.Copysign(y, R[0][1]), math.Copysign(z, R[0][2])
		} else if y >= z {
			x, z = math.Copysign(x, R[0][1]), math.Copysign(z, R[1][2])
		} else {
			x, y = math.Copysign(x, R[0][2]), math.Copysign(y, R[1][2])
		}
		return scale(unit(vec3{x, y, z}), th)
	}
	k := vec3{R[2][1] - R[1][2], R[0][2] - R[2][0], R[1][0] - R[0][1]}
	return scale(k, th/(2*math.Sin(th)))
}

func quatToMat(q [4]float64) mat3 {
	w, x, y, z := q[0], q[1], q[2], q[3]
	return mat3{
		{w*w + x*x - y*y - z*z, 2 * (x*y - w*z), 2 * (x*z + w*y)},
		{2 * (x*y + w*z), w*w - x*x + y*y - z*z, 2 * (y*z - w*x)},
		{2 * (x*z - w*y), 2 * (y*z + w*x), w*w - x*x - y*y + z*z},
	}
}

// jacobiEigen4 restituisce autovalori e autovettori (colonne di V) di una
// matrice simmetrica 4×4.
func jacobiEigen4(a [4][4]float64) (vals [4]float64, V [4][4]float64) {
	for i := range V {
		V[i][i] = 1
	}
	for sweep := 0; sweep < 64; sweep++ {
		off := 0.0
		for p := 0; p < 4; p++ {
			for q := p + 1; q < 4; q++ {
				off += a[p][q] * a[p][q]
			}
		}
		if off < 1e-30 {
			break
		}
		for p := 0; p < 4; p++ {
			for q := p + 1; q < 4; q++ {
				if math.Abs(a[p][q]) < 1e-300 {
					continue
				}
				theta := (a[q][q] - a[p][p]) / (2 * a[p][q])
				t := math.Copysign(1, theta) / (math.Abs(theta) + math.Sqrt(theta*theta+1))
				c := 1 / math.Sqrt(t*t+1)
				s := t * c
				for k := 0; k < 4; k++ {
					akp, akq := a[k][p], a[k][q]
					a[k][p], a[k][q] = c*akp-s*akq, s*akp+c*akq
				}
				for k := 0; k < 4; k++ {
					apk, aqk := a[p][k], a[q][k]
					a[p][k], a[q][k] = c*apk-s*aqk, s*apk+c*aqk
				}
				for k := 0; k < 4; k++ {
					vkp, vkq := V[k][p], V[k][q]
					V[k][p], V[k][q] = c*vkp-s*vkq, s*vkp+c*vkq
				}
			}
		}
	}
	for i := range vals {
		vals[i] = a[i][i]
	}
	return vals, V
}

// procrustes trova la similitudine dst ≈ s·R·src + t che minimizza l'errore
// quadratico pesato (metodo dei quaternioni di Horn, 1987).
func procrustes(src, dst []vec3, w []float64) (R mat3, s float64, t vec3) {
	var ws float64
	var ms, md vec3
	for i := range src {
		ws += w[i]
		ms = add(ms, scale(src[i], w[i]))
		md = add(md, scale(dst[i], w[i]))
	}
	ms, md = scale(ms, 1/ws), scale(md, 1/ws)
	var S mat3 // S[a][b] = Σ w·src_a·dst_b
	var ss float64
	for i := range src {
		a, b := sub(src[i], ms), sub(dst[i], md)
		for r := 0; r < 3; r++ {
			for c := 0; c < 3; c++ {
				S[r][c] += w[i] * a[r] * b[c]
			}
		}
		ss += w[i] * dot(a, a)
	}
	xx, xy, xz := S[0][0], S[0][1], S[0][2]
	yx, yy, yz := S[1][0], S[1][1], S[1][2]
	zx, zy, zz := S[2][0], S[2][1], S[2][2]
	N := [4][4]float64{
		{xx + yy + zz, yz - zy, zx - xz, xy - yx},
		{yz - zy, xx - yy - zz, xy + yx, zx + xz},
		{zx - xz, xy + yx, -xx + yy - zz, yz + zy},
		{xy - yx, zx + xz, yz + zy, -xx - yy + zz},
	}
	vals, V := jacobiEigen4(N)
	best := 0
	for i := 1; i < 4; i++ {
		if vals[i] > vals[best] {
			best = i
		}
	}
	q := [4]float64{V[0][best], V[1][best], V[2][best], V[3][best]}
	R = quatToMat(q)
	var num float64
	for i := range src {
		a, b := sub(src[i], ms), sub(dst[i], md)
		num += w[i] * dot(b, R.apply(a))
	}
	s = num / ss
	t = sub(md, scale(R.apply(ms), s))
	return R, s, t
}

// headToScreen: con la testa in posa neutra (rivolta allo schermo, eretta)
// gli assi del modello canonico (x verso la sinistra del soggetto, y in alto,
// z davanti al viso) valgono −X, +Y, −Z nel sistema dello schermo.
var headToScreen = mat3{{-1, 0, 0}, {0, 1, 0}, {0, 0, -1}}

// Euler scompone la rotazione della testa nel sistema dello schermo
// (destrorso: +X a destra, +Y in alto, +Z verso l'utente) come
// R = Ry(yaw)·Rx(pitch)·Rz(roll), dove R porta la posa neutra in quella
// attuale. Con questo ordine il pitch è l'elevazione della direzione in cui
// guarda il viso (positivo verso l'alto) e lo yaw il suo azimut (positivo
// verso la sinistra dell'utente), come nel resto del driver.
func Euler(R mat3) (pitch, yaw, roll float64) {
	pitch = math.Asin(math.Max(-1, math.Min(1, -R[1][2])))
	yaw = math.Atan2(R[0][2], R[2][2])
	roll = math.Atan2(R[1][0], R[1][1])
	return
}
