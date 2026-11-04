package sink

// LSLReport riassume una ricezione di prova da un outlet LSL.
type LSLReport struct {
	Name, Type     string
	Channels       int
	Labels         []string
	NominalHz      float64
	Samples        int
	MeasuredHz     float64
	NaN            []int     // campioni NaN per canale
	Last           []float32 // ultimo campione
	MedianAgeMs    float64   // ritardo tra timestamp del campione e ricezione (orologio LSL)
	TimeCorrection float64   // s
	GapsOver50ms   int       // pause tra campioni consecutive > 50 ms
}
