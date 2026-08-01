package tlwire

import (
	"bytes"
	"io"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/gzip"
	"github.com/klauspost/compress/zstd"
)

// Compressor is one content-encoding candidate. Only encodings a browser can
// decode natively are worth testing: whatever we pick rides Content-Encoding,
// so the JS side does nothing.
type Compressor struct {
	Name       string
	Encoding   string // the Content-Encoding token
	Compress   func([]byte) []byte
	Decompress func([]byte) []byte
}

func Compressors() []Compressor {
	return []Compressor{
		{Name: "none", Encoding: "", Compress: func(b []byte) []byte { return b }, Decompress: func(b []byte) []byte { return b }},
		{Name: "gzip-1", Encoding: "gzip", Compress: gzipAt(gzip.BestSpeed), Decompress: gunzip},
		{Name: "gzip-6", Encoding: "gzip", Compress: gzipAt(gzip.DefaultCompression), Decompress: gunzip},
		{Name: "zstd-fast", Encoding: "zstd", Compress: zstdAt(zstd.SpeedFastest), Decompress: unzstd},
		{Name: "zstd-def", Encoding: "zstd", Compress: zstdAt(zstd.SpeedDefault), Decompress: unzstd},
		{Name: "br-4", Encoding: "br", Compress: brotliAt(4), Decompress: unbrotli},
		{Name: "br-9", Encoding: "br", Compress: brotliAt(9), Decompress: unbrotli},
	}
}

func gzipAt(level int) func([]byte) []byte {
	return func(b []byte) []byte {
		var out bytes.Buffer
		w, _ := gzip.NewWriterLevel(&out, level)
		_, _ = w.Write(b)
		_ = w.Close()
		return out.Bytes()
	}
}

func gunzip(b []byte) []byte {
	r, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		panic(err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		panic(err)
	}
	return out
}

func zstdAt(level zstd.EncoderLevel) func([]byte) []byte {
	return func(b []byte) []byte {
		w, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(level))
		if err != nil {
			panic(err)
		}
		out := w.EncodeAll(b, nil)
		_ = w.Close()
		return out
	}
}

func unzstd(b []byte) []byte {
	r, err := zstd.NewReader(nil)
	if err != nil {
		panic(err)
	}
	defer r.Close()
	out, err := r.DecodeAll(b, nil)
	if err != nil {
		panic(err)
	}
	return out
}

func brotliAt(level int) func([]byte) []byte {
	return func(b []byte) []byte {
		var out bytes.Buffer
		w := brotli.NewWriterLevel(&out, level)
		_, _ = w.Write(b)
		_ = w.Close()
		return out.Bytes()
	}
}

func unbrotli(b []byte) []byte {
	out, err := io.ReadAll(brotli.NewReader(bytes.NewReader(b)))
	if err != nil {
		panic(err)
	}
	return out
}
