package keel

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// defaultJSON is the codec of a Config that sets no JSON: the standard library's encoding/json,
// which since Go 1.27 runs on the encoding/json/v2 engine with v1's behavior, so a value encodes
// to the bytes encoding/json has always given it (HTML escaped, map keys sorted) and decodes as it
// always has.
var defaultJSON = JSONCodec{
	Marshal:         json.Marshal,
	Unmarshal:       json.Unmarshal,
	UnmarshalNumber: unmarshalNumber,
}

var errTrailingJSON = errors.New("keel: invalid JSON: data after the top-level value")

// unmarshalNumber decodes data into v as json.Unmarshal does, except that numbers decoded into
// interface values are json.Number, keeping their literal.
func unmarshalNumber(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errTrailingJSON
	}
	return nil
}

// resolveJSON returns c with each function it leaves nil taken from defaultJSON.
func resolveJSON(c *JSONCodec) *JSONCodec {
	out := defaultJSON
	if c == nil {
		return &out
	}
	if c.Marshal != nil {
		out.Marshal = c.Marshal
	}
	if c.Unmarshal != nil {
		out.Unmarshal = c.Unmarshal
	}
	if c.UnmarshalNumber != nil {
		out.UnmarshalNumber = c.UnmarshalNumber
	}
	return &out
}

// JSON returns the codec db encodes and decodes json and jsonb values with: Config.JSON, with
// each function it leaves nil taken from the default, the standard library's encoding/json. Packages built on keel use it to read JSON the way the application configured.
func (db *DB) JSON() JSONCodec {
	if db == nil || db.cfg.JSON == nil {
		return defaultJSON
	}
	return *db.cfg.JSON
}

func (db *DB) marshalJSON(v any) ([]byte, error) {
	return db.JSON().Marshal(v)
}
