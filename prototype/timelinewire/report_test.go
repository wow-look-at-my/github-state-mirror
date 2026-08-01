package tlwire

import (
	"fmt"
	"os"
	"testing"
	"time"
)

type Codec interface {
	Name() string
	Encode([]Event) ([]byte, error)
	Decode([]byte) ([]Event, error)
}

func codecs() []Codec { return []Codec{JSONCodec{}, BSONCodec{}, ProtoCodec{}, RowCodec{}, ColCodec{}} }

const (
	nEvents = 100_000 // reqtimeline.DefaultMaxEvents — the full window the endpoint dumps today
	window  = 24 * time.Hour
)

// TestRoundTrip proves every codec is lossless for the fields the chart reads
// before any size number is quoted. A format that loses a field is not a
// smaller format, it is a wrong one.
func TestRoundTrip(t *testing.T) {
	c := Generate(5000, window, 7)
	for _, cd := range codecs() {
		b, err := cd.Encode(c.Events)
		if err != nil {
			t.Fatalf("%s encode: %v", cd.Name(), err)
		}
		got, err := cd.Decode(b)
		if err != nil {
			t.Fatalf("%s decode: %v", cd.Name(), err)
		}
		if len(got) != len(c.Events) {
			t.Fatalf("%s: got %d events, want %d", cd.Name(), len(got), len(c.Events))
		}
		for i := range got {
			want := c.Events[i]
			// Every candidate carries ms-resolution time (JSON's RFC3339Nano
			// is the only one with more, and the chart parses it to ms).
			want.Start = want.Start.Truncate(time.Millisecond)
			g := got[i]
			g.Start = g.Start.Truncate(time.Millisecond).UTC()
			if g != want {
				t.Fatalf("%s: event %d round-tripped as\n %+v\nwant\n %+v", cd.Name(), i, g, want)
			}
		}
	}
}

// TestReport prints the size/speed table. `go test -run TestReport -v`.
func TestReport(t *testing.T) {
	for _, sz := range []struct {
		n    int
		span time.Duration
		what string
	}{
		{nEvents, window, "full ring — what the uncursored endpoint dumps today"},
		{4200, time.Hour, "one hour — what the chart actually paints on first load"},
	} {
		reportOne(t, sz.n, sz.span, sz.what)
	}
}

func reportOne(t *testing.T, nEvents int, window time.Duration, what string) {
	c := Generate(nEvents, window, 7)
	fmt.Printf("\n=== %s ===\n", what)
	fmt.Printf("corpus: %d events over %s (%d repos, %d actors, %d route shapes)\n\n",
		len(c.Events), window, c.Repos, c.Actors, len(routeShapes))

	type row struct {
		codec, comp                       string
		raw, size                         int
		encMs, cmpMs, decmpMs, decMs, tMs float64
	}
	var rows []row

	for _, cd := range codecs() {
		raw, err := cd.Encode(c.Events)
		if err != nil {
			t.Fatal(err)
		}
		encMs := timeIt(func() { _, _ = cd.Encode(c.Events) })
		decMs := timeIt(func() { _, _ = cd.Decode(raw) })
		for _, cp := range Compressors() {
			out := cp.Compress(raw)
			cmpMs := timeIt(func() { cp.Compress(raw) })
			decmpMs := timeIt(func() { cp.Decompress(out) })
			if got := len(cp.Decompress(out)); got != len(raw) {
				t.Fatalf("%s/%s: decompress len %d != %d", cd.Name(), cp.Name, got, len(raw))
			}
			rows = append(rows, row{cd.Name(), cp.Name, len(raw), len(out), encMs, cmpMs, decmpMs, decMs, encMs + cmpMs})
		}
	}

	fmt.Printf("%-10s %-10s %12s %12s %9s %8s %8s %9s %8s\n",
		"codec", "compress", "raw", "on-wire", "B/event", "enc ms", "cmp ms", "decmp ms", "dec ms")
	fmt.Println(dashes(96))
	for _, r := range rows {
		fmt.Printf("%-10s %-10s %12s %12s %9.1f %8.1f %8.1f %9.1f %8.1f\n",
			r.codec, r.comp, human(r.raw), human(r.size), float64(r.size)/float64(nEvents),
			r.encMs, r.cmpMs, r.decmpMs, r.decMs)
	}

	// Leave the two candidate payloads on disk for the browser-side bench.
	if dir := os.Getenv("TLWIRE_DUMP"); dir != "" {
		j, _ := (JSONCodec{}).Encode(c.Events)
		col, _ := (ColCodec{}).Encode(c.Events)
		rw, _ := (RowCodec{}).Encode(c.Events)
		_ = os.WriteFile(dir+"/events.row", rw, 0o644)
		_ = os.WriteFile(dir+"/events.json", j, 0o644)
		_ = os.WriteFile(dir+"/events.col", col, 0o644)
	}
}

func timeIt(f func()) float64 {
	// Best of 3: the smallest run is the one least disturbed by GC/scheduling.
	best := time.Duration(1<<62 - 1)
	for i := 0; i < 3; i++ {
		t0 := time.Now()
		f()
		if d := time.Since(t0); d < best {
			best = d
		}
	}
	return float64(best.Microseconds()) / 1000
}

func human(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.2f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

func dashes(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = '-'
	}
	return string(b)
}
