package tlwire

import "encoding/json"

// JSONCodec is today's wire format: encoding/json over []Event, timestamps as
// RFC3339Nano strings.
type JSONCodec struct{}

func (JSONCodec) Name() string { return "json" }

func (JSONCodec) Encode(ev []Event) ([]byte, error) { return json.Marshal(ev) }

func (JSONCodec) Decode(b []byte) ([]Event, error) {
	var out []Event
	err := json.Unmarshal(b, &out)
	return out, err
}
