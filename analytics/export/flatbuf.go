package export

import "encoding/binary"

// fbObject is a flatbuffer object: a table, a string or a vector. encode appends it to the
// builder and returns the position its references point to.
type fbObject interface {
	encode(b *fbBuilder) int
}

// fbBuilder lays a flatbuffer out front to back: an object is written before the objects it
// refers to, so every reference is a forward unsigned offset, as the format requires.
type fbBuilder struct {
	buf []byte
}

// fbSlot is one field of a table: a scalar of size bytes, or a reference to an object.
type fbSlot struct {
	size int
	bits uint64
	ref  fbObject
}

// fbTable is a table whose field i is slots[i]; a zero slot is an absent field.
type fbTable []fbSlot

// fbString is a string object.
type fbString string

// fbTables is a vector of tables.
type fbTables []fbTable

// fbStructs is a vector of structs of 8-byte alignment, already encoded.
type fbStructs struct {
	count int
	data  []byte
}

// fbBool is a bool field.
func fbBool(v bool) fbSlot {
	if v {
		return fbSlot{size: 1, bits: 1}
	}
	return fbSlot{size: 1}
}

// fbU8 is a ubyte field.
func fbU8(v uint8) fbSlot { return fbSlot{size: 1, bits: uint64(v)} }

// fbI16 is a short field.
func fbI16(v int16) fbSlot { return fbSlot{size: 2, bits: uint64(uint16(v))} }

// fbI32 is an int field.
func fbI32(v int32) fbSlot { return fbSlot{size: 4, bits: uint64(uint32(v))} }

// fbI64 is a long field.
func fbI64(v int64) fbSlot { return fbSlot{size: 8, bits: uint64(v)} }

// fbRef is a field referring to an object.
func fbRef(o fbObject) fbSlot { return fbSlot{size: 4, ref: o} }

// finish encodes root as a whole flatbuffer, padded to 8 bytes.
func (b *fbBuilder) finish(root fbObject) []byte {
	b.buf = append(b.buf[:0], 0, 0, 0, 0)
	pos := root.encode(b)
	binary.LittleEndian.PutUint32(b.buf, uint32(pos))
	b.pad(8)
	return b.buf
}

// pad aligns the end of the buffer.
func (b *fbBuilder) pad(align int) {
	for len(b.buf)%align != 0 {
		b.buf = append(b.buf, 0)
	}
}

// grow appends n zero bytes and returns where they start.
func (b *fbBuilder) grow(n int) int {
	at := len(b.buf)
	b.buf = append(b.buf, make([]byte, n)...)
	return at
}

// encode writes the vtable, then the table with its fields aligned by size, then the objects it
// refers to.
func (t fbTable) encode(b *fbBuilder) int {
	b.pad(2)
	vt := b.grow(4 + 2*len(t))
	b.pad(8)
	start := len(b.buf)
	offsets := make([]int, len(t))
	size := 4
	for _, width := range [...]int{8, 4, 2, 1} {
		for i, s := range t {
			if s.size == width {
				size = (size + width - 1) / width * width
				offsets[i] = size
				size += width
			}
		}
	}
	b.grow(size)
	binary.LittleEndian.PutUint32(b.buf[start:], uint32(start-vt))
	binary.LittleEndian.PutUint16(b.buf[vt:], uint16(4+2*len(t)))
	binary.LittleEndian.PutUint16(b.buf[vt+2:], uint16(size))
	for i, s := range t {
		binary.LittleEndian.PutUint16(b.buf[vt+4+2*i:], uint16(offsets[i]))
		if s.size > 0 && s.ref == nil {
			putScalar(b.buf[start+offsets[i]:], s.size, s.bits)
		}
	}
	for i, s := range t {
		if s.ref != nil {
			b.patch(start+offsets[i], s.ref.encode(b))
		}
	}
	return start
}

// putScalar writes a little-endian scalar of size bytes.
func putScalar(dst []byte, size int, bits uint64) {
	switch size {
	case 1:
		dst[0] = byte(bits)
	case 2:
		binary.LittleEndian.PutUint16(dst, uint16(bits))
	case 4:
		binary.LittleEndian.PutUint32(dst, uint32(bits))
	default:
		binary.LittleEndian.PutUint64(dst, bits)
	}
}

// patch points the reference at position at to the object at position target.
func (b *fbBuilder) patch(at, target int) {
	binary.LittleEndian.PutUint32(b.buf[at:], uint32(target-at))
}

// encode writes the length, the bytes and a terminating zero.
func (s fbString) encode(b *fbBuilder) int {
	b.pad(4)
	at := b.grow(4)
	binary.LittleEndian.PutUint32(b.buf[at:], uint32(len(s)))
	b.buf = append(b.buf, s...)
	b.buf = append(b.buf, 0)
	return at
}

// encode writes the length and one reference per table, then the tables.
func (v fbTables) encode(b *fbBuilder) int {
	b.pad(4)
	at := b.grow(4 + 4*len(v))
	binary.LittleEndian.PutUint32(b.buf[at:], uint32(len(v)))
	for i, t := range v {
		b.patch(at+4+4*i, t.encode(b))
	}
	return at
}

// encode writes the length so that the structs after it are 8-byte aligned.
func (v fbStructs) encode(b *fbBuilder) int {
	for (len(b.buf)+4)%8 != 0 {
		b.buf = append(b.buf, 0)
	}
	at := b.grow(4)
	binary.LittleEndian.PutUint32(b.buf[at:], uint32(v.count))
	b.buf = append(b.buf, v.data...)
	return at
}
