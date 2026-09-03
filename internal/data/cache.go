package data

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"os"
	"path/filepath"
)

// The cache is a columnar binary file, written once from the CSVs and read
// by every later tool. Parsing 590K rows of a 394-column CSV takes seconds;
// reading this file takes tens of milliseconds, which matters because the
// backtester and the experiments reload the data constantly.
//
// Layout, all integers little-endian:
//
//	magic "RGCACHE\x01"
//	uvarint len, fingerprint bytes
//	byte    synthetic (0 or 1)
//	uvarint n (rows)
//	int64   ID[n], DT[n]
//	float64 Amount[n], Card1[n], Addr1[n], Addr2[n], Dist1[n], D1[n] (IEEE bits, NaN kept)
//	int8    IsFraud[n]
//	7 string columns (ProductCode, Card4, Card6, PEmail, REmail, DeviceType,
//	DeviceInfo), each a dictionary then codes:
//	        uvarint k, k x (uvarint len, bytes), uint32 code[n]
//	uint32  CRC-32 (IEEE) of everything above
//
// Strings are dictionary-coded, so a loaded table shares one copy of each
// distinct value instead of holding 590K copies of "gmail.com".
const cacheMagic = "RGCACHE\x01"

// CacheMeta is stored alongside the rows.
type CacheMeta struct {
	Fingerprint string // identifies the source CSVs; see Load
	Synthetic   bool
}

var errCorrupt = errors.New("data cache: corrupt or truncated")

func numCols(t *Txn) [6]*float64 {
	return [6]*float64{&t.Amount, &t.Card1, &t.Addr1, &t.Addr2, &t.Dist1, &t.D1}
}

func strCols(t *Txn) [7]*string {
	return [7]*string{&t.ProductCode, &t.Card4, &t.Card6, &t.PEmail, &t.REmail, &t.DeviceType, &t.DeviceInfo}
}

// WriteCache encodes txns (in their current order) to w.
func WriteCache(w io.Writer, txns []Txn, meta CacheMeta) error {
	h := crc32.NewIEEE()
	bw := bufio.NewWriterSize(io.MultiWriter(w, h), 1<<20)
	var scratch [binary.MaxVarintLen64]byte
	putUvarint := func(v uint64) {
		bw.Write(binary.AppendUvarint(scratch[:0], v))
	}
	put64 := func(v uint64) {
		bw.Write(binary.LittleEndian.AppendUint64(scratch[:0], v))
	}

	bw.WriteString(cacheMagic)
	putUvarint(uint64(len(meta.Fingerprint)))
	bw.WriteString(meta.Fingerprint)
	if meta.Synthetic {
		bw.WriteByte(1)
	} else {
		bw.WriteByte(0)
	}
	putUvarint(uint64(len(txns)))

	for i := range txns {
		put64(uint64(txns[i].ID))
	}
	for i := range txns {
		put64(uint64(txns[i].DT))
	}
	for c := range 6 {
		for i := range txns {
			put64(math.Float64bits(*numCols(&txns[i])[c]))
		}
	}
	for i := range txns {
		bw.WriteByte(byte(txns[i].IsFraud))
	}
	for c := range 7 {
		codes := make(map[string]uint32)
		var dict []string
		for i := range txns {
			s := *strCols(&txns[i])[c]
			if _, ok := codes[s]; !ok {
				codes[s] = uint32(len(dict))
				dict = append(dict, s)
			}
		}
		putUvarint(uint64(len(dict)))
		for _, s := range dict {
			putUvarint(uint64(len(s)))
			bw.WriteString(s)
		}
		for i := range txns {
			bw.Write(binary.LittleEndian.AppendUint32(scratch[:0], codes[*strCols(&txns[i])[c]]))
		}
	}
	if err := bw.Flush(); err != nil {
		return err
	}
	_, err := w.Write(binary.LittleEndian.AppendUint32(nil, h.Sum32()))
	return err
}

// ReadCache decodes a cache produced by WriteCache.
func ReadCache(b []byte) ([]Txn, CacheMeta, error) {
	var meta CacheMeta
	if len(b) < len(cacheMagic)+4 || string(b[:len(cacheMagic)]) != cacheMagic {
		return nil, meta, fmt.Errorf("data cache: bad magic (stale format?)")
	}
	body, sum := b[:len(b)-4], binary.LittleEndian.Uint32(b[len(b)-4:])
	if crc32.ChecksumIEEE(body) != sum {
		return nil, meta, errCorrupt
	}
	d := decoder{b: body[len(cacheMagic):]}
	meta.Fingerprint = string(d.bytes(int(d.uvarint())))
	meta.Synthetic = d.byte() == 1
	n := int(d.uvarint())
	if d.err != nil || n < 0 || n > len(d.b) {
		return nil, meta, errCorrupt
	}
	txns := make([]Txn, n)
	for i := range txns {
		txns[i].ID = int64(d.u64())
	}
	for i := range txns {
		txns[i].DT = int64(d.u64())
	}
	for c := range 6 {
		for i := range txns {
			*numCols(&txns[i])[c] = math.Float64frombits(d.u64())
		}
	}
	for i := range txns {
		txns[i].IsFraud = int8(d.byte())
	}
	for c := range 7 {
		k := int(d.uvarint())
		if d.err != nil || k > len(d.b) {
			return nil, meta, errCorrupt
		}
		dict := make([]string, k)
		for j := range dict {
			dict[j] = string(d.bytes(int(d.uvarint())))
		}
		for i := range txns {
			code := d.u32()
			if int(code) >= len(dict) {
				return nil, meta, errCorrupt
			}
			*strCols(&txns[i])[c] = dict[code]
		}
	}
	if d.err != nil || len(d.b) != 0 {
		return nil, meta, errCorrupt
	}
	return txns, meta, nil
}

// WriteCacheFile writes the cache atomically: to a temporary file in the
// same directory, then renamed over path, so a crash never leaves a
// half-written cache that a later run would trust.
func WriteCacheFile(path string, txns []Txn, meta CacheMeta) (err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(f.Name())
		}
	}()
	if err = f.Chmod(0o644); err != nil { // CreateTemp makes it 0600
		return err
	}
	if err = WriteCache(f, txns, meta); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// ReadCacheFile reads a cache written by WriteCacheFile.
func ReadCacheFile(path string) ([]Txn, CacheMeta, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, CacheMeta{}, err
	}
	txns, meta, err := ReadCache(b)
	if err != nil {
		return nil, meta, fmt.Errorf("%s: %w", path, err)
	}
	return txns, meta, nil
}

// decoder reads fixed-width and varint values with a sticky error, so the
// column loops stay free of error checks.
type decoder struct {
	b   []byte
	err error
}

func (d *decoder) take(n int) []byte {
	if d.err != nil || n < 0 || n > len(d.b) {
		d.err = errCorrupt
		return nil
	}
	v := d.b[:n]
	d.b = d.b[n:]
	return v
}

func (d *decoder) u64() uint64 {
	if b := d.take(8); b != nil {
		return binary.LittleEndian.Uint64(b)
	}
	return 0
}

func (d *decoder) u32() uint32 {
	if b := d.take(4); b != nil {
		return binary.LittleEndian.Uint32(b)
	}
	return 0
}

func (d *decoder) byte() byte {
	if b := d.take(1); b != nil {
		return b[0]
	}
	return 0
}

func (d *decoder) bytes(n int) []byte { return d.take(n) }

func (d *decoder) uvarint() uint64 {
	if d.err != nil {
		return 0
	}
	v, n := binary.Uvarint(d.b)
	if n <= 0 {
		d.err = errCorrupt
		return 0
	}
	d.b = d.b[n:]
	return v
}
