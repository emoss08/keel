// Package sqlw builds statement text and its bind arguments.
package sqlw

import (
	"strconv"
	"sync"
)

// W accumulates statement text and arguments. The zero value is ready to use.
type W struct {
	buf  []byte
	args []any
}

var pool = sync.Pool{New: func() any { return &W{buf: make([]byte, 0, 1024)} }}

// Get returns a writer from the pool.
func Get() *W { return pool.Get().(*W) }

// Put returns w to the pool. The caller must not use w, its String or its Args afterwards.
func Put(w *W) {
	if cap(w.buf) > 64<<10 || cap(w.args) > 4096 {
		return
	}
	w.buf = w.buf[:0]
	clear(w.args)
	w.args = w.args[:0]
	pool.Put(w)
}

// S appends s.
func (w *W) S(s string) *W {
	w.buf = append(w.buf, s...)
	return w
}

// B appends one byte.
func (w *W) B(c byte) *W {
	w.buf = append(w.buf, c)
	return w
}

// Int appends n in decimal.
func (w *W) Int(n int64) *W {
	w.buf = strconv.AppendInt(w.buf, n, 10)
	return w
}

// Arg binds v and appends its placeholder.
func (w *W) Arg(v any) *W {
	w.args = append(w.args, v)
	w.buf = append(w.buf, '$')
	w.buf = strconv.AppendInt(w.buf, int64(len(w.args)), 10)
	return w
}

// Len returns the length of the text written so far.
func (w *W) Len() int { return len(w.buf) }

// NArgs returns the number of bound arguments.
func (w *W) NArgs() int { return len(w.args) }

// String returns a copy of the statement text.
func (w *W) String() string { return string(w.buf) }

// Args returns a copy of the bound arguments.
func (w *W) Args() []any {
	if len(w.args) == 0 {
		return nil
	}
	out := make([]any, len(w.args))
	copy(out, w.args)
	return out
}

// Truncate cuts the text back to n bytes; arguments are kept.
func (w *W) Truncate(n int) { w.buf = w.buf[:n] }
