package stream

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"strconv"
)

// Store keeps one snapshot per stateful task: the task's state and the input
// offset it covers. A task that moves to another process on a rebalance is
// restored from it there, which is how state is handed over. DirStore keeps
// snapshots in a directory every process can reach, which on one machine is
// a local directory and in production would be an object store.
type Store interface {
	// Put replaces the snapshot of (group, partition) atomically.
	Put(group string, partition int32, offset int64, state []byte) error
	// Get returns the latest snapshot, or ok false if there is none.
	Get(group string, partition int32) (offset int64, state []byte, ok bool, err error)
}

// DirStore is a Store in a directory.
type DirStore struct{ Dir string }

const storeMagic = "RGSTRM01"

func (s DirStore) path(group string, partition int32) string {
	return filepath.Join(s.Dir, group, strconv.Itoa(int(partition))+".snap")
}

// Put writes a temporary file, fsyncs it, renames it over the old snapshot
// and fsyncs the directory, so a crash leaves the old snapshot or the new
// one, never a mixture.
func (s DirStore) Put(group string, partition int32, offset int64, state []byte) (err error) {
	path := s.path(group, partition)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".snap-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(f.Name())
		}
	}()
	w := bufio.NewWriterSize(f, 1<<20)
	crc := crc32.NewIEEE()
	head := append([]byte(storeMagic), binary.AppendVarint(nil, offset)...)
	for _, b := range [][]byte{head, state} {
		crc.Write(b)
		if _, err = w.Write(b); err != nil {
			return err
		}
	}
	if _, err = w.Write(binary.LittleEndian.AppendUint32(nil, crc.Sum32())); err != nil {
		return err
	}
	if err = w.Flush(); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// Get reads and checks a snapshot.
func (s DirStore) Get(group string, partition int32) (int64, []byte, bool, error) {
	path := s.path(group, partition)
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil, false, nil
	}
	if err != nil {
		return 0, nil, false, err
	}
	if len(b) < len(storeMagic)+5 || string(b[:len(storeMagic)]) != storeMagic {
		return 0, nil, false, fmt.Errorf("stream: %s is not a snapshot", path)
	}
	body, sum := b[:len(b)-4], binary.LittleEndian.Uint32(b[len(b)-4:])
	if crc32.ChecksumIEEE(body) != sum {
		return 0, nil, false, fmt.Errorf("stream: %s: checksum mismatch", path)
	}
	offset, n := binary.Varint(body[len(storeMagic):])
	if n <= 0 {
		return 0, nil, false, fmt.Errorf("stream: %s: bad offset", path)
	}
	return offset, body[len(storeMagic)+n:], true, nil
}
