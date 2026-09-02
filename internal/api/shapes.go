package api

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

// Passthrough SHAPE capture: what the request-group counters (requestgroups.go)
// deliberately do not hold.
//
// The groups answer "which uncached route is hottest and why". Modeling that
// route then needs the part the log throws away: what the request actually
// looked like (query parameter names, Accept, who sends it) and — the piece
// nothing recorded until now — what GitHub ANSWERS with, so the trimmed rebuild
// can be designed without guessing GitHub's schema from memory. This store
// samples that, per route shape, and `GET /api/brief` renders it as a ready-to-
// act implementation brief.
//
// Values are never recorded. A body sample is reduced to its JSON SKELETON —

const (
	// shapeStoreCap bounds the map; past it, new shapes are dropped and known ones keep updating.
	shapeStoreCap = 300
	// shapeResampleAfter re-samples to catch a schema variant, not for freshness.
	shapeResampleAfter = 30 * time.Minute
	// shapeMaxSampleBytes caps buffering; a larger body streams through unsampled rather than lie via truncation.
	shapeMaxSampleBytes = 256 << 10
	// shapeMaxDecodedBytes bounds gzip decode; JSON expands far past the wire cap, and the source is trusted (GitHub).
	shapeMaxDecodedBytes = 4 << 20
	// shapeMaxPaths / shapeMaxNames bound the per-shape detail sets.
	shapeMaxPaths = 3
	shapeMaxNames = 24
	// Skeleton bounds guard against a dynamic-key object or a deep document.
	skeletonMaxDepth   = 6
	skeletonMaxKeys    = 40
	skeletonMaxKeyLen  = 64
	skeletonMaxRunes   = 8000
	skeletonIndentUnit = "  "
)

// routeShape is everything captured about (method, route shape) that a
// cached-route implementation would otherwise have to be reverse-engineered
// from the calling service's source.
type routeShape struct {
	method string
	route  string
	seen   int64
	// queryNames counts how often each query parameter name appears; never values.
	queryNames map[string]int64
	accepts    map[string]int64
	// callers are the verified display names or principal keys that send this request.
	callers map[string]int64
	// statuses tallies upstream answers; a dominant is a cacheable verdict, not a failure.
	statuses map[int]int64
	// samplePaths are a few concrete paths, for recognizing the resource.
	samplePaths []string

	// bodies holds the captured response per status: skeleton, content type, size.
	bodies map[int]*bodySample
	// nonJSON records a sampled body confirmed not JSON, distinct from never sampled.
	nonJSON      map[int]*nonJSONSample
	lastSampleAt time.Time
}

type bodySample struct {
	Status      int    `json:"status"`
	ContentType string `json:"content_type,omitempty"`
	Bytes       int    `json:"bytes"`
	Skeleton    string `json:"skeleton"`
	At          string `json:"at"`
}

// nonJSONSample records confirmed-non-JSON response: enough to explain
// why no skeleton exists, never the bytes themselves.
type nonJSONSample struct {
	Status      int    `json:"status"`
	ContentType string `json:"content_type,omitempty"`
	Bytes       int    `json:"bytes"`
	At          string `json:"at"`
}

// shapeStore holds routeShapes in memory; it resets on restart, never touching the schema.
type shapeStore struct {
	mu     sync.Mutex
	shapes map[string]*routeShape
}

func newShapeStore() *shapeStore { return &shapeStore{shapes: make(map[string]*routeShape)} }

// wantsBody resamples when nothing is captured yet, or the last capture aged out.
// see docs/dashboard/implementation-brief.md
func (s *shapeStore) wantsBody(method, route string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sh := s.shapes[method+" "+route]
	if sh == nil {
		return len(s.shapes) < shapeStoreCap
	}
	return time.Since(sh.lastSampleAt) > shapeResampleAfter
}

// observation is recorded passthrough. Body is the buffered response body
// when it was sampled (nil otherwise) and is consumed here — never retained.
type observation struct {
	Method          string
	Route           string
	Path            string
	QueryNames      []string
	Accept          string
	Caller          string
	Status          int
	ContentType     string
	ContentEncoding string
	Body            []byte
}

func (s *shapeStore) observe(o observation) {
	if s == nil || o.Route == "" {
		return
	}
	key := o.Method + " " + o.Route
	attempted := len(o.Body) > 0
	skeleton := ""
	if attempted {
		skeleton = jsonSkeleton(decodeSample(o.Body, o.ContentEncoding))
	}
	now := time.Now().UTC()

	s.mu.Lock()
	defer s.mu.Unlock()
	sh := s.shapes[key]
	if sh == nil {
		if len(s.shapes) >= shapeStoreCap {
			return
		}
		sh = &routeShape{
			method: o.Method, route: o.Route,
			queryNames: map[string]int64{}, accepts: map[string]int64{},
			callers: map[string]int64{}, statuses: map[int]int64{},
			bodies: map[int]*bodySample{}, nonJSON: map[int]*nonJSONSample{},
		}
		s.shapes[key] = sh
	}
	sh.seen++
	sh.statuses[o.Status]++
	bumpBounded(sh.queryNames, o.QueryNames...)
	if a := strings.TrimSpace(o.Accept); a != "" {
		bumpBounded(sh.accepts, clampRoute(a))
	}
	if o.Caller != "" {
		bumpBounded(sh.callers, o.Caller)
	}
	if len(sh.samplePaths) < shapeMaxPaths && !containsString(sh.samplePaths, o.Path) {
		sh.samplePaths = append(sh.samplePaths, clampRoute(o.Path))
	}
	if !attempted {
		return
	}
	// Advances whether or not it parsed as JSON: a confirmed-non-JSON status is
	sh.lastSampleAt = now
	if skeleton != "" {
		delete(sh.nonJSON, o.Status)
		sh.bodies[o.Status] = &bodySample{
			Status: o.Status, ContentType: o.ContentType,
			Bytes: len(o.Body), Skeleton: skeleton, At: now.Format(time.RFC3339),
		}
		// Keep the per-status body map bounded the same way as everything
		// else: statuses are few, but a pathological upstream must not grow it.
		if len(sh.bodies) > shapeMaxNames {
			for k := range sh.bodies {
				delete(sh.bodies, k)
				break
			}
		}
		return
	}
	delete(sh.bodies, o.Status)
	sh.nonJSON[o.Status] = &nonJSONSample{
		Status: o.Status, ContentType: o.ContentType,
		Bytes: len(o.Body), At: now.Format(time.RFC3339),
	}
	if len(sh.nonJSON) > shapeMaxNames {
		for k := range sh.nonJSON {
			delete(sh.nonJSON, k)
			break
		}
	}
}

// bumpBounded increments counters for keys, refusing to grow the map past
// shapeMaxNames (known keys keep counting).
func bumpBounded(m map[string]int64, keys ...string) {
	for _, k := range keys {
		if k == "" {
			continue
		}
		if _, ok := m[k]; !ok && len(m) >= shapeMaxNames {
			continue
		}
		m[k]++
	}
}

func containsString(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

// routeShapeSnapshot is shape in the brief payload.
type routeShapeSnapshot struct {
	Key         string          `json:"key"`
	Method      string          `json:"method"`
	Route       string          `json:"route"`
	Seen        int64           `json:"seen"`
	QueryNames  []countedName   `json:"query_names,omitempty"`
	Accepts     []countedName   `json:"accepts,omitempty"`
	Callers     []countedName   `json:"callers,omitempty"`
	Statuses    []countedInt    `json:"statuses,omitempty"`
	SamplePaths []string        `json:"sample_paths,omitempty"`
	Bodies      []bodySample    `json:"bodies,omitempty"`
	NonJSON     []nonJSONSample `json:"non_json,omitempty"`
}

type countedName struct {
	Name  string `json:"name"`
	Count int64  `json:"count"`
}

type countedInt struct {
	Value int   `json:"value"`
	Count int64 `json:"count"`
}

func (s *shapeStore) snapshot() map[string]routeShapeSnapshot {
	out := map[string]routeShapeSnapshot{}
	if s == nil {
		return out
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, sh := range s.shapes {
		snap := routeShapeSnapshot{
			Key: key, Method: sh.method, Route: sh.route, Seen: sh.seen,
			QueryNames: sortedCounts(sh.queryNames),
			Accepts:    sortedCounts(sh.accepts),
			Callers:    sortedCounts(sh.callers),
			Statuses:   sortedIntCounts(sh.statuses),
		}
		snap.SamplePaths = append(snap.SamplePaths, sh.samplePaths...)
		for _, b := range sh.bodies {
			snap.Bodies = append(snap.Bodies, *b)
		}
		sort.Slice(snap.Bodies, func(i, j int) bool { return snap.Bodies[i].Status < snap.Bodies[j].Status })
		for _, n := range sh.nonJSON {
			snap.NonJSON = append(snap.NonJSON, *n)
		}
		sort.Slice(snap.NonJSON, func(i, j int) bool { return snap.NonJSON[i].Status < snap.NonJSON[j].Status })
		out[key] = snap
	}
	return out
}

func sortedCounts(m map[string]int64) []countedName {
	out := make([]countedName, 0, len(m))
	for k, v := range m {
		out = append(out, countedName{Name: k, Count: v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func sortedIntCounts(m map[int]int64) []countedInt {
	out := make([]countedInt, 0, len(m))
	for k, v := range m {
		out = append(out, countedInt{Value: k, Count: v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Value < out[j].Value })
	return out
}

// decodeSample gunzips a captured sample when the upstream Content-Encoding
// says gzip; every other value passes the bytes through unchanged.
// see docs/dashboard/implementation-brief.md, "Compressed samples"
func decodeSample(body []byte, contentEncoding string) []byte {
	if !strings.Contains(strings.ToLower(contentEncoding), "gzip") {
		return body
	}
	gz, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return body
	}
	defer gz.Close()
	decoded, err := io.ReadAll(io.LimitReader(gz, shapeMaxDecodedBytes))
	if err != nil && len(decoded) == 0 {
		return body
	}
	return decoded
}

// jsonSkeleton reduces a document to its key/type outline; no value ever survives.
// see docs/dashboard/implementation-brief.md
func jsonSkeleton(body []byte) string {
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return ""
	}
	var sb strings.Builder
	writeSkeleton(&sb, v, 0)
	out := sb.String()
	if len([]rune(out)) > skeletonMaxRunes {
		return string([]rune(out)[:skeletonMaxRunes]) + "\n… (truncated)"
	}
	return out
}

func writeSkeleton(sb *strings.Builder, v any, depth int) {
	if depth > skeletonMaxDepth {
		sb.WriteString("…")
		return
	}
	switch t := v.(type) {
	case map[string]any:
		if len(t) == 0 {
			sb.WriteString("{}")
			return
		}
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		truncated := false
		if len(keys) > skeletonMaxKeys {
			keys, truncated = keys[:skeletonMaxKeys], true
		}
		sb.WriteString("{\n")
		pad := strings.Repeat(skeletonIndentUnit, depth+1)
		for _, k := range keys {
			sb.WriteString(pad)
			sb.WriteString(clampKey(k))
			sb.WriteString(": ")
			writeSkeleton(sb, t[k], depth+1)
			sb.WriteString("\n")
		}
		if truncated {
			sb.WriteString(pad + "… (more keys)\n")
		}
		sb.WriteString(strings.Repeat(skeletonIndentUnit, depth) + "}")
	case []any:
		if len(t) == 0 {
			sb.WriteString("[] (empty)")
			return
		}
		sb.WriteString("[")
		writeSkeleton(sb, t[0], depth+1)
		sb.WriteString("] × ")
		sb.WriteString(itoa(len(t)))
	case string:
		sb.WriteString("string")
	case float64:
		sb.WriteString("number")
	case bool:
		sb.WriteString("bool")
	case nil:
		// A nullable field's key must still be emitted, so null is load-bearing.
		sb.WriteString("null")
	default:
		sb.WriteString("unknown")
	}
}

func clampKey(k string) string {
	if len(k) > skeletonMaxKeyLen {
		return k[:skeletonMaxKeyLen] + "…"
	}
	return k
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
