package ir

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

func strictDecode(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("unexpected data after the JSON value")
	}
	return nil
}

// Decode reads a stored definition of any supported version, upgrading it to Version. Unknown
// keys are rejected and numbers are kept exact (as json.Number). It does not validate.
func Decode(data []byte) (*Definition, error) {
	var head struct {
		Version   *int `json:"version"`
		IRVersion *int `json:"irVersion"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return nil, Errors{{Code: CodeJSON, Message: "the definition is not a JSON object: " + err.Error(), Pointer: ""}}
	}
	switch {
	case head.Version == nil && head.IRVersion != nil:
		def, err := UpgradeFromTrenovaV1(data)
		if err != nil {
			return nil, err
		}
		return &def, nil
	case head.Version == nil:
		return nil, Errors{{Code: CodeRequired, Message: "the definition has no version", Pointer: "/version"}}
	case *head.Version != Version:
		return nil, Errors{{Code: CodeUnsupportedVersion, Message: fmt.Sprintf("definition version %d is not supported; this keel reads version %d", *head.Version, Version), Pointer: "/version"}}
	}
	def := new(Definition)
	if err := strictDecode(data, def); err != nil {
		return nil, Errors{{Code: CodeJSON, Message: "the definition does not have the expected shape: " + err.Error()}}
	}
	return def, nil
}

// Encode writes a definition as indented JSON with a trailing newline.
func Encode(def *Definition) ([]byte, error) {
	out, err := json.MarshalIndent(def, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// Count is a whole number or the name of an int parameter that supplies it. It encodes as a
// number, or as {"param": "name"}.
type Count struct {
	// Value is the number.
	Value int
	// Param names the parameter instead.
	Param string
}

// N returns a Count holding a number.
func N(n int) Count { return Count{Value: n} }

// CountParam returns a Count supplied by a parameter.
func CountParam(name string) Count { return Count{Param: name} }

// IsZero reports whether the count is unset.
func (c Count) IsZero() bool { return c.Value == 0 && c.Param == "" }

type paramRef struct {
	Param string `json:"param"`
}

// MarshalJSON encodes a number or a parameter reference.
func (c Count) MarshalJSON() ([]byte, error) {
	if c.Param != "" {
		return json.Marshal(paramRef{c.Param})
	}
	return json.Marshal(c.Value)
}

// UnmarshalJSON decodes a whole number or {"param": "name"}.
func (c *Count) UnmarshalJSON(data []byte) error {
	if bytes.HasPrefix(bytes.TrimSpace(data), []byte("{")) {
		var p paramRef
		if err := strictDecode(data, &p); err != nil {
			return err
		}
		*c = Count{Param: p.Param}
		return nil
	}
	var n int
	if err := json.Unmarshal(data, &n); err != nil {
		return errors.New("expected a whole number or {\"param\": \"name\"}")
	}
	*c = Count{Value: n}
	return nil
}

// Grain is a bucket, or the name of a string parameter that chooses one when the report runs.
// It encodes as "month", or as {"param": "name"}.
type Grain struct {
	// Unit is the bucket.
	Unit Bucket
	// Param names the parameter instead.
	Param string
}

// Every returns a Grain holding a bucket.
func Every(b Bucket) Grain { return Grain{Unit: b} }

// GrainParam returns a Grain chosen by a parameter.
func GrainParam(name string) Grain { return Grain{Param: name} }

// IsZero reports whether the grain is unset.
func (g Grain) IsZero() bool { return g.Unit == "" && g.Param == "" }

// MarshalJSON encodes a bucket name or a parameter reference.
func (g Grain) MarshalJSON() ([]byte, error) {
	if g.Param != "" {
		return json.Marshal(paramRef{g.Param})
	}
	return json.Marshal(g.Unit)
}

// UnmarshalJSON decodes a bucket name or {"param": "name"}.
func (g *Grain) UnmarshalJSON(data []byte) error {
	if bytes.HasPrefix(bytes.TrimSpace(data), []byte("{")) {
		var p paramRef
		if err := strictDecode(data, &p); err != nil {
			return err
		}
		*g = Grain{Param: p.Param}
		return nil
	}
	var b Bucket
	if err := json.Unmarshal(data, &b); err != nil {
		return errors.New("expected a bucket name or {\"param\": \"name\"}")
	}
	*g = Grain{Unit: b}
	return nil
}

// Values is a list of values or the name of a multi-value parameter that supplies them. It
// encodes as an array, or as {"param": "name"}.
type Values struct {
	// List holds the values.
	List []string
	// Param names the parameter instead.
	Param string
}

// IsZero reports whether neither a list nor a parameter is set.
func (v Values) IsZero() bool { return v.List == nil && v.Param == "" }

// MarshalJSON encodes an array or a parameter reference.
func (v Values) MarshalJSON() ([]byte, error) {
	if v.Param != "" {
		return json.Marshal(paramRef{v.Param})
	}
	return json.Marshal(v.List)
}

// UnmarshalJSON decodes an array of strings or {"param": "name"}.
func (v *Values) UnmarshalJSON(data []byte) error {
	if bytes.HasPrefix(bytes.TrimSpace(data), []byte("{")) {
		var p paramRef
		if err := strictDecode(data, &p); err != nil {
			return err
		}
		*v = Values{Param: p.Param}
		return nil
	}
	var list []string
	if err := json.Unmarshal(data, &list); err != nil {
		return errors.New("expected an array of values or {\"param\": \"name\"}")
	}
	if list == nil {
		list = []string{}
	}
	*v = Values{List: list}
	return nil
}
