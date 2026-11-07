package telemetry

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/ablescia/tobii4c-linux/internal/gaze"
	"github.com/ablescia/tobii4c-linux/internal/ttp"
)

// cell è una colonna del firmware con la sua eventuale colonna di validità.
type cell struct {
	key, valid uint32 // 0 = assente
}

// colRow è una riga della tabella dello stream gaze: una colonna generale
// (c[0]) o una coppia sinistro/destro.
type colRow struct {
	name     string // nome del firmware senza "Left"/"Right"
	label    string // descrizione in italiano
	unit     string
	eye      bool
	status   bool // stato dell'occhio (0 tracciato, 4 non rilevato)
	c        [2]cell
	exported bool // il driver la esporta in UDP/LSL
}

// colInfo descrive le colonne note: etichetta, unità e numero di decimali.
type colInfo struct {
	label, unit string
}

// known associa il nome del firmware (senza lato, minuscolo) alla descrizione.
var known = map[string]colInfo{
	"timestamp":                               {"Timestamp del dispositivo", "µs"},
	"relative combined gaze point":            {"Sguardo combinato, filtrato", "norm"},
	"remote gaze point responsive combined":   {"Sguardo combinato, reattivo", "norm"},
	"gaze origin tracker":                     {"Centro della cornea, sistema del tracker", "mm"},
	"gaze origin display":                     {"Centro della cornea, sistema dello schermo", "mm"},
	"eye 3d relative position":                {"Posizione nel track box", "norm"},
	"eye 3d gaze point":                       {"Punto di sguardo 3D", "mm"},
	"eye 2d gaze point":                       {"Punto di sguardo sullo schermo", "norm"},
	"eye pupil diameter":                      {"Diametro della pupilla", "mm"},
	"eye validity":                            {"Stato dell'occhio", ""},
	"eyeball center position":                 {"Centro del bulbo oculare", "mm"},
	"eye 2d position relative to sensor area": {"Posizione sul sensore", "norm"},
	"user 3d position guide":                  {"Guida al posizionamento", "norm"},
	"absolute eye openness":                   {"Apertura della palpebra", "mm"},
}

func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix) {
		return s[len(prefix):], true
	}
	return s, false
}

func cutSuffixFold(s, suffix string) (string, bool) {
	if len(s) >= len(suffix) && strings.EqualFold(s[len(s)-len(suffix):], suffix) {
		return s[:len(s)-len(suffix)], true
	}
	return s, false
}

// buildColumns ordina le colonne dichiarate dal firmware (op 0x4ba) in righe:
// le colonne "Left …"/"Right …" con lo stesso nome diventano una riga a due
// valori, e ogni colonna "… Validity" si unisce alla colonna di cui è la
// validità (stesso nome, o l'unica il cui nome la prolunga). L'ordine è
// quello del firmware, alla prima comparsa.
func buildColumns(cols []ttp.Record, km gaze.KeyMap) []colRow {
	exported := map[uint32]bool{}
	for _, k := range km.Fields() {
		if k != 0 {
			exported[k] = true
		}
	}
	type parsed struct {
		key   uint32
		side  int // -1 generale, 0 sinistro, 1 destro
		base  string
		valid bool
	}
	ps := make([]parsed, 0, len(cols))
	for _, r := range cols {
		p := parsed{key: r.ID, side: -1, base: strings.TrimSpace(r.Name)}
		if b, ok := cutPrefixFold(p.base, "Left "); ok {
			p.side, p.base = 0, b
		} else if b, ok := cutPrefixFold(p.base, "Right "); ok {
			p.side, p.base = 1, b
		}
		if b, ok := cutSuffixFold(p.base, " Validity"); ok && b != "" && !strings.EqualFold(p.base, "Eye Validity") {
			p.base, p.valid = b, true
		}
		ps = append(ps, p)
	}
	var rows []colRow
	index := map[string]int{} // "g:"/"e:" + nome minuscolo → riga
	keyOf := func(p parsed) string {
		if p.side < 0 {
			return "g:" + strings.ToLower(p.base)
		}
		return "e:" + strings.ToLower(p.base)
	}
	for _, p := range ps {
		if p.valid {
			continue
		}
		k := keyOf(p)
		if p.base == "" { // colonna senza nome: una riga a sé
			k = fmt.Sprintf("g:#%x", p.key)
		}
		i, ok := index[k]
		if !ok {
			r := colRow{name: p.base, eye: p.side >= 0}
			if info, ok := known[strings.ToLower(p.base)]; ok {
				r.label, r.unit = info.label, info.unit
			}
			r.status = strings.EqualFold(p.base, "Eye Validity")
			if p.base == "" {
				r.name = "(senza nome)"
				r.label = unnamedLabel(p.key, km)
			}
			rows = append(rows, r)
			i = len(rows) - 1
			index[k] = i
		}
		side := max(p.side, 0)
		rows[i].c[side].key = p.key
		rows[i].exported = rows[i].exported || exported[p.key]
	}
	// validità: stesso nome, altrimenti l'unica riga il cui nome la prolunga
	for _, p := range ps {
		if !p.valid {
			continue
		}
		side := max(p.side, 0)
		i, ok := index[keyOf(p)]
		if !ok {
			cand := -1
			for j, r := range rows {
				if r.eye == (p.side >= 0) && r.c[side].key != 0 && r.c[side].valid == 0 &&
					strings.HasPrefix(strings.ToLower(r.name), strings.ToLower(p.base)+" ") {
					if cand >= 0 {
						cand = -2 // ambigua
						break
					}
					cand = j
				}
			}
			if cand < 0 {
				// nessuna corrispondenza: riga propria
				rows = append(rows, colRow{name: p.base + " Validity", label: "", eye: p.side >= 0})
				i = len(rows) - 1
				rows[i].c[side].key = p.key
				index[keyOf(p)+" validity"] = i
				continue
			}
			i = cand
		}
		rows[i].c[side].valid = p.key
	}
	return rows
}

// unnamedLabel descrive le colonne che il firmware non nomina.
func unnamedLabel(key uint32, km gaze.KeyMap) string {
	if key == km.FrameCounter {
		return "Contatore dei frame del dispositivo"
	}
	if key == 0x11 {
		return "Contatore 0x11, cresce di 1 a frame"
	}
	return "Colonna senza nome"
}

// ---------------------------------------------------------------------------
// Formattazione

// num formatta v con d decimali e la virgola decimale.
func num(v float64, d int) string {
	s := strconv.FormatFloat(v, 'f', d, 64)
	if strings.Trim(s, "-0.") == "" {
		s = strings.TrimPrefix(s, "-")
	}
	return strings.Replace(s, ".", ",", 1)
}

// numW è num allineato a destra su w caratteri.
func numW(v float64, d, w int) string {
	s := num(v, d)
	if n := w - len(s); n > 0 {
		s = strings.Repeat(" ", n) + s
	}
	return s
}

// decimals per unità: mm con un decimale, coordinate normalizzate con tre.
func decimals(unit string) (d, w int) {
	switch unit {
	case "mm":
		return 1, 6
	case "norm":
		return 3, 6
	}
	return 3, 7
}

// fmtValue formatta il valore grezzo di una colonna.
func fmtValue(v ttp.Value, unit string) string {
	d, w := decimals(unit)
	switch v.Kind {
	case ttp.KindU32:
		return strconv.FormatUint(uint64(v.U), 10)
	case ttp.KindS64:
		return strconv.FormatInt(v.I, 10)
	case ttp.KindFixed16, ttp.KindFixed42:
		if unit == "mm" {
			return num(v.F[0], 2)
		}
		return num(v.F[0], 3)
	case ttp.KindPoint:
		parts := make([]string, v.N)
		for i := range parts {
			parts[i] = numW(v.F[i], d, w)
		}
		return strings.Join(parts, " ")
	case ttp.KindString:
		return strconv.Quote(string(v.B))
	case ttp.KindBytes:
		if len(v.B) == 0 {
			return "[0 byte]"
		}
		return fmt.Sprintf("[%d] %x", len(v.B), v.B[:min(len(v.B), 12)])
	case ttp.KindNone:
		return "—"
	}
	return v.Kind.String()
}

// statusText descrive lo stato dell'occhio.
func statusText(v uint32) string {
	switch v {
	case 0:
		return "0 tracciato"
	case 4:
		return "4 non rilevato"
	}
	return strconv.FormatUint(uint64(v), 10)
}
