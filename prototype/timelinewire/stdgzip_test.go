package tlwire

import (
	"bytes"
	stdgzip "compress/gzip"
	"fmt"
	"testing"
	"time"
)

// Does the STDLIB gzip (what the server would use without a new dependency)
// hold the klauspost numbers on the columnar payload?
func TestStdlibGzip(t *testing.T) {
	c := Generate(nEvents, window, 7)
	for _, cd := range []Codec{ColCodec{}, JSONCodec{}} {
		raw, _ := cd.Encode(c.Events)
		for _, lvl := range []int{1, 6, 9} {
			var out bytes.Buffer
			best := time.Hour
			for i := 0; i < 3; i++ {
				out.Reset()
				t0 := time.Now()
				w, _ := stdgzip.NewWriterLevel(&out, lvl)
				_, _ = w.Write(raw)
				_ = w.Close()
				if d := time.Since(t0); d < best {
					best = d
				}
			}
			fmt.Printf("%-9s stdlib gzip-%d: %10s  in %6.1f ms\n", cd.Name(), lvl, human(out.Len()), float64(best.Microseconds())/1000)
		}
	}
}
