package backtest

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"slices"
	"unsafe"

	"github.com/Sanjith-Shan/RiskGate/internal/schema"
)

// The table file is a flat little-endian dump of the columns, so loading is
// a sequence of large reads straight into the columns' own memory, with no
// parsing and no per-row work. That lets the service start from a cached
// feature table in a fraction of a second instead of replaying the feature
// engine over 590K payments.
//
//	magic      8 bytes  "RGTABLE\x01"
//	N          u64
//	fields     u32 count, then per field: kind u8, name (u32 length + bytes)
//	flags      u8       bit 0: LabelTime present
//	ID, DT     N x i64 each
//	Amount     N x f64
//	Fraud      N x i8
//	LabelTime  N x i64, if flagged
//	numbers    per number column in slot order: N x f64 (raw IEEE bits, so
//	           NaN, -0 and infinities survive exactly)
//	strings    per string column in slot order: u32 dictionary size, each
//	           entry as u32 length + bytes (entry 0 is ""), then N x u32 codes
//	crc        u32      CRC-32C of every byte before it
//
// The field list is stored so that a table built for an older catalog is
// rejected with a clear message instead of being read with shifted columns.

var magic = [8]byte{'R', 'G', 'T', 'A', 'B', 'L', 'E', 1}

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

const flagLabelTime = 1

// ErrCorrupt reports a table file that is truncated, malformed or fails its
// checksum.
var ErrCorrupt = errors.New("backtest: table file is corrupt")

// fixed is the element types stored as raw column dumps.
type fixed interface {
	~int8 | ~uint32 | ~int64 | ~float64
}

// rawBytes views a column as its bytes in memory, without copying. This is
// the one use of unsafe in the package, and it is what makes loading a
// 250 MB table a matter of reads rather than a decode loop per value. The
// file is little-endian; on a big-endian machine every column is
// byte-swapped after reading and before writing.
func rawBytes[T fixed](s []T) []byte {
	if len(s) == 0 {
		return nil
	}
	var zero T
	return unsafe.Slice((*byte)(unsafe.Pointer(unsafe.SliceData(s))), len(s)*int(unsafe.Sizeof(zero)))
}

var littleEndian = binary.NativeEndian.Uint16([]byte{1, 0}) == 1

// swapWidth reverses every width-byte element of b in place.
func swapWidth(b []byte, width int) {
	for i := 0; i+width <= len(b); i += width {
		slices.Reverse(b[i : i+width])
	}
}

// Save writes t to path atomically (a temporary file renamed into place), so
// a reader never sees a half-written cache.
func (t *Table) Save(path string) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".table-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	if err = t.Encode(tmp); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// crcWriter checksums everything written through it.
type crcWriter struct {
	w   *bufio.Writer
	crc uint32
	err error
	tmp [8]byte
}

func (c *crcWriter) write(b []byte) {
	if c.err == nil {
		c.crc = crc32.Update(c.crc, castagnoli, b)
		_, c.err = c.w.Write(b)
	}
}

func (c *crcWriter) u8(v uint8)   { c.tmp[0] = v; c.write(c.tmp[:1]) }
func (c *crcWriter) u32(v uint32) { c.write(binary.LittleEndian.AppendUint32(c.tmp[:0], v)) }
func (c *crcWriter) u64(v uint64) { c.write(binary.LittleEndian.AppendUint64(c.tmp[:0], v)) }
func (c *crcWriter) str(s string) { c.u32(uint32(len(s))); c.write([]byte(s)) }

func writeCol[T fixed](c *crcWriter, s []T) {
	b := rawBytes(s)
	var zero T
	if w := int(unsafe.Sizeof(zero)); !littleEndian && w > 1 {
		b = bytes.Clone(b)
		swapWidth(b, w)
	}
	c.write(b)
}

// Encode writes t in the table file format.
func (t *Table) Encode(w io.Writer) error {
	if err := t.Validate(); err != nil {
		return err
	}
	c := &crcWriter{w: bufio.NewWriterSize(w, 1<<20)}
	c.write(magic[:])
	c.u64(uint64(t.N))
	fields := t.Catalog.Fields()
	c.u32(uint32(len(fields)))
	for _, f := range fields {
		c.u8(uint8(f.Kind))
		c.str(f.Name)
	}
	var flags uint8
	if t.LabelTime != nil {
		flags |= flagLabelTime
	}
	c.u8(flags)
	writeCol(c, t.ID)
	writeCol(c, t.DT)
	writeCol(c, t.Amount)
	writeCol(c, t.Fraud)
	writeCol(c, t.LabelTime)
	for _, col := range t.Num {
		writeCol(c, col)
	}
	for i, col := range t.Str {
		c.u32(uint32(len(t.Dict[i])))
		for _, s := range t.Dict[i] {
			c.str(s)
		}
		writeCol(c, col)
	}
	if c.err != nil {
		return c.err
	}
	var sum [4]byte
	binary.LittleEndian.PutUint32(sum[:], c.crc)
	if _, err := c.w.Write(sum[:]); err != nil {
		return err
	}
	return c.w.Flush()
}

// Load reads a table file written by Save and checks it against cat.
func Load(path string, cat *schema.Catalog) (*Table, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	t, err := decode(f, st.Size(), cat)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return t, nil
}

// Decode parses a table file held in memory.
func Decode(b []byte, cat *schema.Catalog) (*Table, error) {
	return decode(bytes.NewReader(b), int64(len(b)), cat)
}

// crcReader checksums every byte read through it and tracks how many
// remain before the trailing checksum, so that a corrupt length is caught
// before anything of that size is allocated.
type crcReader struct {
	r    *bufio.Reader
	crc  uint32
	left int64
	err  error
	tmp  [8]byte
}

func (c *crcReader) read(b []byte) {
	if c.err != nil {
		return
	}
	if int64(len(b)) > c.left {
		c.err = fmt.Errorf("%w: truncated", ErrCorrupt)
		return
	}
	if _, err := io.ReadFull(c.r, b); err != nil {
		c.err = fmt.Errorf("%w: %v", ErrCorrupt, err)
		return
	}
	c.left -= int64(len(b))
	c.crc = crc32.Update(c.crc, castagnoli, b)
}

func (c *crcReader) u8() uint8   { c.read(c.tmp[:1]); return c.tmp[0] }
func (c *crcReader) u32() uint32 { c.read(c.tmp[:4]); return binary.LittleEndian.Uint32(c.tmp[:4]) }
func (c *crcReader) u64() uint64 { c.read(c.tmp[:8]); return binary.LittleEndian.Uint64(c.tmp[:8]) }

// count checks that n elements of width bytes can still be in the file and
// returns n as an int.
func (c *crcReader) count(n uint64, width int) int {
	if c.err == nil && n > uint64(c.left)/uint64(width) {
		c.err = fmt.Errorf("%w: length %d past the end of the file", ErrCorrupt, n)
	}
	if c.err != nil {
		return 0
	}
	return int(n)
}

func (c *crcReader) str() string {
	b := make([]byte, c.count(uint64(c.u32()), 1))
	c.read(b)
	return string(b)
}

func readCol[T fixed](c *crcReader, n int) []T {
	var zero T
	width := int(unsafe.Sizeof(zero))
	if c.count(uint64(n), width); c.err != nil {
		return nil
	}
	s := make([]T, n)
	b := rawBytes(s)
	c.read(b)
	if !littleEndian && width > 1 {
		swapWidth(b, width)
	}
	return s
}

func decode(r io.Reader, size int64, cat *schema.Catalog) (*Table, error) {
	notTable := errors.New("backtest: not a RiskGate table file (or an older format version)")
	if size < int64(len(magic))+4 {
		return nil, notTable
	}
	c := &crcReader{r: bufio.NewReaderSize(r, 1<<20), left: size - 4}
	var m [len(magic)]byte
	if c.read(m[:]); c.err != nil || m != magic {
		return nil, notTable
	}
	n := c.count(c.u64(), 8)
	nf := c.count(uint64(c.u32()), 5)
	fields := cat.Fields()
	if c.err == nil && nf != len(fields) {
		return nil, fmt.Errorf("backtest: table has %d fields, catalog has %d; rebuild the table", nf, len(fields))
	}
	for i := 0; i < nf && c.err == nil; i++ {
		kind, name := schema.Kind(c.u8()), c.str()
		if c.err == nil && (name != fields[i].Name || kind != fields[i].Kind) {
			return nil, fmt.Errorf("backtest: table field %d is %s %q, catalog has %s %q; rebuild the table",
				i, kind, name, fields[i].Kind, fields[i].Name)
		}
	}
	flags := c.u8()
	t := &Table{Catalog: cat, N: n}
	t.ID = readCol[int64](c, n)
	t.DT = readCol[int64](c, n)
	t.Amount = readCol[float64](c, n)
	t.Fraud = readCol[int8](c, n)
	if flags&flagLabelTime != 0 {
		t.LabelTime = readCol[int64](c, n)
	}
	t.Num = make([][]float64, cat.NumCount())
	for i := range t.Num {
		t.Num[i] = readCol[float64](c, n)
	}
	t.Str = make([][]uint32, cat.StrCount())
	t.Dict = make([][]string, cat.StrCount())
	for i := range t.Str {
		d := make([]string, c.count(uint64(c.u32()), 4))
		for j := range d {
			d[j] = c.str()
		}
		t.Dict[i] = d
		t.Str[i] = readCol[uint32](c, n)
	}
	if c.err != nil {
		return nil, c.err
	}
	if c.left != 0 {
		return nil, fmt.Errorf("%w: %d trailing bytes", ErrCorrupt, c.left)
	}
	var sum [4]byte
	if _, err := io.ReadFull(c.r, sum[:]); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	if binary.LittleEndian.Uint32(sum[:]) != c.crc {
		return nil, fmt.Errorf("%w: checksum mismatch", ErrCorrupt)
	}
	if err := t.Validate(); err != nil {
		return nil, err
	}
	return t, nil
}
