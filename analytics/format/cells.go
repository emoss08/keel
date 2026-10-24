package format

// Cells holds the rendered cells of a column batch in one buffer, so rendering a batch allocates
// nothing once the buffers have grown. Reset it to reuse it for the next batch.
type Cells struct {
	buf   []byte
	ends  []int
	tones []Tone
}

// Reset empties the cells, keeping their buffers.
func (c *Cells) Reset() {
	c.buf = c.buf[:0]
	c.ends = c.ends[:0]
	c.tones = c.tones[:0]
}

// Len returns the number of cells.
func (c *Cells) Len() int { return len(c.ends) }

// Bytes returns cell i's text. It aliases the buffer and is valid until the next Reset.
func (c *Cells) Bytes(i int) []byte {
	start := 0
	if i > 0 {
		start = c.ends[i-1]
	}
	return c.buf[start:c.ends[i]]
}

// String returns cell i's text as a string.
func (c *Cells) String(i int) string { return string(c.Bytes(i)) }

// Tone returns cell i's tone.
func (c *Cells) Tone(i int) Tone { return c.tones[i] }
