package tlwire

import (
	"os"
	"testing"
)

// TestDump writes the full-ring payloads for the browser-side bench
// (bench.mjs). `TLWIRE_DUMP=. go test -run TestDump`.
func TestDump(t *testing.T) {
	dir := os.Getenv("TLWIRE_DUMP")
	if dir == "" {
		t.Skip("set TLWIRE_DUMP to write payloads")
	}
	c := Generate(nEvents, window, 7)
	for name, enc := range map[string]Codec{"events.json": JSONCodec{}, "events.col": ColCodec{}, "events.row": RowCodec{}} {
		b, err := enc.Encode(c.Events)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dir+"/"+name, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
