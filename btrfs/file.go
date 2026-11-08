package btrfs

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"

	"github.com/klauspost/compress/zstd"
)

const (
	compNone = 0
	compZlib = 1
	compLZO  = 2
	compZstd = 3
)

type fileExtent struct {
	fileOff  uint64 // logical offset in the file
	length   uint64 // bytes this extent covers in the file
	kind     uint8  // 0 inline, 1 regular, 2 prealloc
	comp     uint8
	inline   []byte // inline data (possibly compressed)
	ramBytes uint64
	diskAddr uint64 // logical address of the on-disk extent (0 = hole)
	diskLen  uint64
	extOff   uint64 // offset inside the decompressed extent
}

// File is an open regular file (or symlink) that supports concurrent ReadAt.
type File struct {
	fs      *FS
	ino     Ino
	size    int64
	extents []fileExtent

	mu       sync.Mutex
	lastAddr uint64
	lastData []byte
}

// OpenFile loads the extent map of a file.
func (f *FS) OpenFile(ino Ino) (*File, error) {
	st, err := f.Stat(ino)
	if err != nil {
		return nil, err
	}
	root, err := f.treeRoot(ino.Tree)
	if err != nil {
		return nil, err
	}
	file := &File{fs: f, ino: ino, size: st.Size}
	le := binary.LittleEndian
	err = f.items(root, ino.Num, typeExtentData, func(k Key, d []byte) error {
		if len(d) < 21 {
			return nil
		}
		e := fileExtent{fileOff: k.Offset, ramBytes: le.Uint64(d[8:]), comp: d[16], kind: d[20]}
		if d[17] != 0 {
			return errors.New("btrfs: encrypted extents are not supported")
		}
		if e.kind == 0 {
			e.inline = d[21:]
			e.length = e.ramBytes
		} else {
			if len(d) < 53 {
				return nil
			}
			e.diskAddr = le.Uint64(d[21:])
			e.diskLen = le.Uint64(d[29:])
			e.extOff = le.Uint64(d[37:])
			e.length = le.Uint64(d[45:])
		}
		file.extents = append(file.extents, e)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(file.extents, func(i, j int) bool { return file.extents[i].fileOff < file.extents[j].fileOff })
	return file, nil
}

// Size returns the file size.
func (fl *File) Size() int64 { return fl.size }

// ReadAt implements io.ReaderAt.
func (fl *File) ReadAt(p []byte, off int64) (int, error) {
	if off >= fl.size {
		return 0, io.EOF
	}
	want := len(p)
	var eof error
	if off+int64(want) > fl.size {
		want = int(fl.size - off)
		eof = io.EOF
	}
	done := 0
	for done < want {
		cur := uint64(off) + uint64(done)
		// last extent starting at or before cur
		i := sort.Search(len(fl.extents), func(i int) bool { return fl.extents[i].fileOff > cur }) - 1
		var e *fileExtent
		if i >= 0 && cur < fl.extents[i].fileOff+fl.extents[i].length {
			e = &fl.extents[i]
		}
		if e == nil {
			// hole up to the next extent (or EOF)
			end := uint64(off) + uint64(want)
			if i+1 < len(fl.extents) && fl.extents[i+1].fileOff < end {
				end = fl.extents[i+1].fileOff
			}
			n := int(end - cur)
			clear(p[done : done+n])
			done += n
			continue
		}
		in := cur - e.fileOff
		n := want - done
		if uint64(n) > e.length-in {
			n = int(e.length - in)
		}
		if err := fl.readExtent(e, p[done:done+n], in); err != nil {
			return done, err
		}
		done += n
	}
	return done, eof
}

func (fl *File) readExtent(e *fileExtent, p []byte, in uint64) error {
	switch {
	case e.kind == 0: // inline
		data := e.inline
		if e.comp != compNone {
			d, err := decompress(e.comp, data, int(e.ramBytes), fl.fs.sb.SectorSize)
			if err != nil {
				return fmt.Errorf("btrfs: inline extent: %w", err)
			}
			data = d
		}
		n := copy(p, safeSlice(data, in))
		clear(p[n:])
		return nil
	case e.kind == 2 || e.diskAddr == 0: // preallocated or hole
		clear(p)
		return nil
	case e.comp == compNone:
		return fl.fs.readLogical(p, e.diskAddr+e.extOff+in)
	}
	data, err := fl.decompressed(e)
	if err != nil {
		return err
	}
	n := copy(p, safeSlice(data, e.extOff+in))
	clear(p[n:])
	return nil
}

func safeSlice(b []byte, off uint64) []byte {
	if off >= uint64(len(b)) {
		return nil
	}
	return b[off:]
}

func (fl *File) decompressed(e *fileExtent) ([]byte, error) {
	fl.mu.Lock()
	if fl.lastAddr == e.diskAddr && fl.lastData != nil {
		d := fl.lastData
		fl.mu.Unlock()
		return d, nil
	}
	fl.mu.Unlock()
	raw := make([]byte, e.diskLen)
	if err := fl.fs.readLogical(raw, e.diskAddr); err != nil {
		return nil, err
	}
	d, err := decompress(e.comp, raw, int(e.ramBytes), fl.fs.sb.SectorSize)
	if err != nil {
		return nil, fmt.Errorf("btrfs: extent %#x: %w", e.diskAddr, err)
	}
	fl.mu.Lock()
	fl.lastAddr, fl.lastData = e.diskAddr, d
	fl.mu.Unlock()
	return d, nil
}

// btrfs pads compressed extents to a sector, so the zstd frame is followed by zeros:
// decode as a stream and stop once the expected number of bytes has been produced.
var zstdPool = sync.Pool{New: func() any {
	d, _ := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(1<<30))
	return d
}}

func decompress(alg uint8, src []byte, ram int, sectorSize uint32) ([]byte, error) {
	switch alg {
	case compZlib:
		zr, err := zlib.NewReader(bytes.NewReader(src))
		if err != nil {
			return nil, err
		}
		out := make([]byte, ram)
		n, err := io.ReadFull(zr, out)
		if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
			return nil, err
		}
		return out[:n], nil
	case compZstd:
		d := zstdPool.Get().(*zstd.Decoder)
		defer zstdPool.Put(d)
		if err := d.Reset(bytes.NewReader(src)); err != nil {
			return nil, err
		}
		out := make([]byte, ram)
		n, err := io.ReadFull(d, out)
		// The last extent of a file holds less than ram_bytes; after its frame
		// only sector padding follows, which the decoder reports as a bad magic.
		if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF && !(n > 0 && errors.Is(err, zstd.ErrMagicMismatch)) {
			return nil, err
		}
		return out[:n], nil
	case compLZO:
		return btrfsLZO(src, ram, int(sectorSize))
	}
	return nil, fmt.Errorf("unknown compression type %d", alg)
}

// btrfsLZO decodes btrfs' LZO framing: a 4-byte total length, then segments of
// (4-byte length + lzo1x data); segment headers never straddle a sector boundary.
func btrfsLZO(src []byte, ram, sector int) ([]byte, error) {
	if len(src) < 4 {
		return nil, errors.New("lzo: short extent")
	}
	if sector <= 0 {
		sector = 4096
	}
	total := int(binary.LittleEndian.Uint32(src))
	if total > len(src) {
		total = len(src)
	}
	out := make([]byte, 0, ram)
	pos := 4
	for pos < total && len(out) < ram {
		if rem := sector - pos%sector; rem < 4 {
			pos += rem
			if pos >= total {
				break
			}
		}
		if pos+4 > total {
			break
		}
		segLen := int(binary.LittleEndian.Uint32(src[pos:]))
		pos += 4
		if segLen == 0 || pos+segLen > len(src) {
			return nil, fmt.Errorf("lzo: bad segment length %d", segLen)
		}
		dec, err := lzo1xDecompress(src[pos:pos+segLen], sector)
		if err != nil {
			return nil, err
		}
		out = append(out, dec...)
		pos += segLen
	}
	if len(out) > ram {
		out = out[:ram]
	}
	return out, nil
}
