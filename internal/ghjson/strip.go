package ghjson

import (
	"bytes"
	"encoding/json"
	"strings"
)

// StripURLFields removes GitHub's verbose URL/link fields from JSON and
// re-encodes the result compactly. Whitespace is intentionally not preserved.
func StripURLFields(body []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v interface{}
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	v = strip(v)
	return json.Marshal(v)
}

func strip(v interface{}) interface{} {
	switch x := v.(type) {
	case map[string]interface{}:
		for k, child := range x {
			if IsURLField(k) {
				delete(x, k)
				continue
			}
			x[k] = strip(child)
		}
		return x
	case []interface{}:
		for i := range x {
			x[i] = strip(x[i])
		}
		return x
	default:
		return v
	}
}

// IsURLField reports whether a JSON object key carries GitHub URL/link noise.
func IsURLField(key string) bool {
	k := strings.ToLower(key)
	return k == "url" || k == "_links" || k == "links" || strings.HasSuffix(k, "_url")
}
