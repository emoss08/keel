package keel

import "github.com/bytedance/sonic"

// defaultJSON is the codec of a Config that sets no JSON: sonic in its encoding/json-compatible
// mode, so a value encodes to the bytes encoding/json would give it (HTML escaped, map keys
// sorted) and decodes as encoding/json would decode it, faster.
var defaultJSON = JSONCodec{
	Marshal:   sonic.ConfigStd.Marshal,
	Unmarshal: sonic.ConfigStd.Unmarshal,
	UnmarshalNumber: sonic.Config{
		EscapeHTML:       true,
		SortMapKeys:      true,
		CompactMarshaler: true,
		CopyString:       true,
		ValidateString:   true,
		UseNumber:        true,
	}.Froze().Unmarshal,
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
// each function it leaves nil taken from the default, sonic in its encoding/json-compatible
// mode. Packages built on keel use it to read JSON the way the application configured.
func (db *DB) JSON() JSONCodec {
	if db == nil || db.cfg.JSON == nil {
		return defaultJSON
	}
	return *db.cfg.JSON
}

func (db *DB) marshalJSON(v any) ([]byte, error) {
	return db.JSON().Marshal(v)
}
