package features

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Snapshots are a small binary format: a magic string naming the state kind
// and version, the configuration, then the contents in a deterministic
// order. Equal states produce byte-identical snapshots, which is what the
// restore tests compare.

var errSnapshot = errors.New("features: corrupt or truncated snapshot")

// encoder writes snapshot values with a sticky error.
type encoder struct {
	w   *bufio.Writer
	err error
	buf [binary.MaxVarintLen64]byte
}

func newEncoder(w io.Writer) *encoder { return &encoder{w: bufio.NewWriterSize(w, 1<<16)} }

func (e *encoder) write(b []byte) {
	if e.err == nil {
		_, e.err = e.w.Write(b)
	}
}

func (e *encoder) uvarint(v uint64) { e.write(binary.AppendUvarint(e.buf[:0], v)) }
func (e *encoder) varint(v int64)   { e.write(binary.AppendVarint(e.buf[:0], v)) }
func (e *encoder) u64(v uint64)     { e.write(binary.LittleEndian.AppendUint64(e.buf[:0], v)) }

func (e *encoder) str(s string) {
	e.uvarint(uint64(len(s)))
	if e.err == nil {
		_, e.err = e.w.WriteString(s)
	}
}

func (e *encoder) key(k Key) {
	e.write(append(e.buf[:0], byte(k.Entity)))
	for _, n := range k.Num {
		e.u64(n)
	}
	e.str(k.Str)
}

func (e *encoder) flush() error {
	if e.err != nil {
		return e.err
	}
	return e.w.Flush()
}

// decoder reads what encoder wrote, with a sticky error.
type decoder struct {
	r   *bufio.Reader
	err error
	buf [8]byte
}

func newDecoder(r io.Reader) *decoder {
	if br, ok := r.(*bufio.Reader); ok {
		return &decoder{r: br}
	}
	return &decoder{r: bufio.NewReaderSize(r, 1<<16)}
}

func (d *decoder) fail(err error) {
	if d.err == nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			err = errSnapshot
		}
		d.err = err
	}
}

func (d *decoder) uvarint() uint64 {
	if d.err != nil {
		return 0
	}
	v, err := binary.ReadUvarint(d.r)
	d.fail(err)
	return v
}

func (d *decoder) varint() int64 {
	if d.err != nil {
		return 0
	}
	v, err := binary.ReadVarint(d.r)
	d.fail(err)
	return v
}

func (d *decoder) u64() uint64 {
	if d.err != nil {
		return 0
	}
	if _, err := io.ReadFull(d.r, d.buf[:8]); err != nil {
		d.fail(err)
		return 0
	}
	return binary.LittleEndian.Uint64(d.buf[:8])
}

// count reads a length and rejects absurd values before anything allocates.
func (d *decoder) count(limit uint64) int {
	n := d.uvarint()
	if n > limit {
		d.fail(errSnapshot)
		return 0
	}
	return int(n)
}

func (d *decoder) str() string {
	n := d.count(1 << 20)
	if d.err != nil {
		return ""
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(d.r, b); err != nil {
		d.fail(err)
		return ""
	}
	return string(b)
}

func (d *decoder) key() Key {
	var k Key
	b, err := d.r.ReadByte()
	if err != nil {
		d.fail(err)
		return k
	}
	if b >= NumEntities {
		d.fail(errSnapshot)
	}
	k.Entity = Entity(b)
	for i := range k.Num {
		k.Num[i] = d.u64()
	}
	k.Str = d.str()
	return k
}

// expectInt checks that a configuration value matches.
func (d *decoder) expectInt(name string, want int64) {
	if got := d.varint(); d.err == nil && got != want {
		d.fail(fmt.Errorf("features: snapshot has %s %d, this state has %d", name, got, want))
	}
}

// end fails if bytes remain after a snapshot that should be complete.
func (d *decoder) end() error {
	if d.err != nil {
		return d.err
	}
	if _, err := d.r.ReadByte(); err != io.EOF {
		return fmt.Errorf("features: trailing bytes after snapshot")
	}
	return nil
}
