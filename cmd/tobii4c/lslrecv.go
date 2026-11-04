package main

import (
	"flag"
	"fmt"
	"math"
	"time"

	"github.com/ablescia/tobii4c-linux/internal/sink"
)

// cmdLSLRecv è un ricevitore LSL di prova: si collega all'outlet del driver e
// ne verifica canali, frequenza e valori.
func cmdLSLRecv(args []string) error {
	fs := flag.NewFlagSet("lsl-recv", flag.ExitOnError)
	name := fs.String("name", "Tobii4C", "nome dell'outlet LSL")
	dur := fs.Duration("duration", 10*time.Second, "durata della ricezione")
	fs.Parse(args)
	r, err := sink.LSLReceive(*name, *dur)
	if err != nil {
		return err
	}
	fmt.Printf("outlet %q tipo %q: %d canali, nominale %.0f Hz\n", r.Name, r.Type, r.Channels, r.NominalHz)
	fmt.Printf("ricevuti %d campioni in %v: %.2f Hz misurati, pause > 50 ms: %d\n", r.Samples, *dur, r.MeasuredHz, r.GapsOver50ms)
	fmt.Printf("ritardo mediano (timestamp → ricezione): %.2f ms, correzione dell'orologio %.3f ms\n", r.MedianAgeMs, r.TimeCorrection*1000)
	fmt.Println("canale                    ultimo valore   NaN")
	for i := 0; i < r.Channels; i++ {
		label := "?"
		if i < len(r.Labels) {
			label = r.Labels[i]
		}
		v := float64(r.Last[i])
		s := fmt.Sprintf("%.4f", v)
		if math.IsNaN(v) {
			s = "NaN"
		}
		fmt.Printf("  %-24s %12s   %5.1f%%\n", label, s, 100*float64(r.NaN[i])/math.Max(1, float64(r.Samples)))
	}
	return nil
}
