package keel

// EmptyJSONObject and EmptyJSONArray are written for a nil map or slice in a non-pointer jsonb
// field, so the column holds {} or [] rather than NULL or JSON null.
var (
	EmptyJSONObject = []byte("{}")
	EmptyJSONArray  = []byte("[]")
)

// Strings converts a slice of a string type to []string.
func Strings[S ~string](vs []S) []string {
	if vs == nil {
		return nil
	}
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = string(v)
	}
	return out
}

// Int64s converts a slice of an int64 type to []int64.
func Int64s[S ~int64](vs []S) []int64 {
	if vs == nil {
		return nil
	}
	out := make([]int64, len(vs))
	for i, v := range vs {
		out[i] = int64(v)
	}
	return out
}

// Int32s converts a slice of an int32 type to []int32.
func Int32s[S ~int32](vs []S) []int32 {
	if vs == nil {
		return nil
	}
	out := make([]int32, len(vs))
	for i, v := range vs {
		out[i] = int32(v)
	}
	return out
}

// Int16s converts a slice of an int16 type to []int16.
func Int16s[S ~int16](vs []S) []int16 {
	if vs == nil {
		return nil
	}
	out := make([]int16, len(vs))
	for i, v := range vs {
		out[i] = int16(v)
	}
	return out
}
