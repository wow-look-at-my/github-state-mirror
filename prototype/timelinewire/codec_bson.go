package tlwire

import "go.mongodb.org/mongo-driver/v2/bson"

// BSONCodec encodes the array as a BSON document {"e": [...]}: BSON has no
// top-level array, which is already a hint that it is a document format being
// used as a list format.
type BSONCodec struct{}

type bsonEnvelope struct {
	E []Event `bson:"e"`
}

func (BSONCodec) Name() string { return "bson" }

func (BSONCodec) Encode(ev []Event) ([]byte, error) { return bson.Marshal(bsonEnvelope{E: ev}) }

func (BSONCodec) Decode(b []byte) ([]Event, error) {
	var out bsonEnvelope
	err := bson.Unmarshal(b, &out)
	return out.E, err
}
