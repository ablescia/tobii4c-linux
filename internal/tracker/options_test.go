package tracker

import (
	"testing"

	"github.com/ablescia/tobii4c-linux/internal/gaze"
)

// Regressione: un chiamante che non imposta KeyMap (es. `calibrate`) deve
// comunque decodificare lo sguardo, altrimenti nessun occhio risulta tracciato.
func TestDefaultsFillKeyMap(t *testing.T) {
	var o Options
	o.defaults()
	if o.KeyMap != gaze.DefaultKeyMap() {
		t.Fatalf("KeyMap not defaulted: %+v", o.KeyMap)
	}
	custom := gaze.DefaultKeyMap()
	custom.Gaze = 0x20
	o = Options{KeyMap: custom}
	o.defaults()
	if o.KeyMap.Gaze != 0x20 {
		t.Fatal("explicit KeyMap overwritten")
	}
}
