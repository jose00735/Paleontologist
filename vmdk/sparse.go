package vmdk

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
)

// sparseHeader is the hosted sparse extent header ("KDMV"), 512 bytes, little endian.
type sparseHeader struct {
	Magic              uint32
	Version            uint32
	Flags              uint32
	Capacity           uint64 // sectors
	GrainSize          uint64 // sectors
	DescriptorOffset   uint64
	DescriptorSize     uint64
	NumGTEsPerGT       uint32
	RgdOffset          uint64
	GdOffset           uint64
	OverHead           uint64
	UncleanShutdown    uint8
	SingleEndLineChar  uint8
	NonEndLineChar     uint8
	DoubleEndLineChar1 uint8
	DoubleEndLineChar2 uint8
	CompressAlgorithm  uint16
}

const (
	flagCompressed = 1 << 16
	gdAtEnd        = 0xffffffffffffffff
)

type sparseExtent struct {
	f      *os.File
	name   string
	h      sparseHeader
	gd     []uint32
	base   int64 // absolute offset of this extent within the virtual disk
	parent Disk  // for unallocated grains of snapshots

	mu    sync.Mutex
	gts   map[uint32][]uint32
	cache *grainCache
}

func readHeader(f *os.File, off int64) (sparseHeader, error) {
	var h sparseHeader
	buf := make([]byte, 512)
	if _, err := f.ReadAt(buf, off); err != nil {
		return h, err
	}
	if err := binary.Read(bytes.NewReader(buf), binary.LittleEndian, &h); err != nil {
		return h, err
	}
	if h.Magic != 0x564d444b {
		return h, errors.New("vmdk: bad sparse header magic")
	}
	return h, nil
}

func openSparse(f *os.File, name string) (*sparseExtent, error) {
	h, err := readHeader(f, 0)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if h.GdOffset == gdAtEnd {
		// streamOptimized: the real header is in the footer, 1024 bytes before the end.
		st, err := f.Stat()
		if err != nil {
			return nil, err
		}
		fh, err := readHeader(f, st.Size()-1024)
		if err != nil {
			return nil, fmt.Errorf("%s: stream-optimized footer: %w", name, err)
		}
		h.GdOffset = fh.GdOffset
		h.RgdOffset = fh.RgdOffset
		if fh.Capacity != 0 {
			h.Capacity = fh.Capacity
		}
		if fh.GrainSize != 0 {
			h.GrainSize = fh.GrainSize
		}
		if fh.NumGTEsPerGT != 0 {
			h.NumGTEsPerGT = fh.NumGTEsPerGT
		}
		h.Flags |= fh.Flags
	}
	if h.GrainSize == 0 || h.GrainSize > 1<<16 || h.NumGTEsPerGT == 0 {
		return nil, fmt.Errorf("%s: invalid sparse geometry", name)
	}
	if h.Flags&flagCompressed != 0 && h.CompressAlgorithm != 1 {
		return nil, fmt.Errorf("%s: unsupported grain compression %d", name, h.CompressAlgorithm)
	}
	gtCoverage := h.GrainSize * uint64(h.NumGTEsPerGT)
	n := (h.Capacity + gtCoverage - 1) / gtCoverage
	gdOff := h.GdOffset
	if gdOff == 0 || gdOff == gdAtEnd {
		gdOff = h.RgdOffset
	}
	buf := make([]byte, n*4)
	if _, err := f.ReadAt(buf, int64(gdOff)*sector); err != nil && err != io.EOF {
		return nil, fmt.Errorf("%s: grain directory: %w", name, err)
	}
	gd := make([]uint32, n)
	for i := range gd {
		gd[i] = binary.LittleEndian.Uint32(buf[i*4:])
	}
	return &sparseExtent{f: f, name: name, h: h, gd: gd, gts: map[uint32][]uint32{}, cache: newGrainCache(64)}, nil
}

func (s *sparseExtent) gt(i uint32) ([]uint32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.gts[i]; ok {
		return t, nil
	}
	if int(i) >= len(s.gd) || s.gd[i] == 0 {
		return nil, nil
	}
	buf := make([]byte, s.h.NumGTEsPerGT*4)
	if _, err := s.f.ReadAt(buf, int64(s.gd[i])*sector); err != nil && err != io.EOF {
		return nil, err
	}
	t := make([]uint32, s.h.NumGTEsPerGT)
	for j := range t {
		t[j] = binary.LittleEndian.Uint32(buf[j*4:])
	}
	s.gts[i] = t
	return t, nil
}

func (s *sparseExtent) ReadAt(p []byte, off int64) (int, error) {
	grainBytes := int64(s.h.GrainSize) * sector
	done := 0
	for done < len(p) {
		cur := off + int64(done)
		g := cur / grainBytes
		in := cur % grainBytes
		n := len(p) - done
		if int64(n) > grainBytes-in {
			n = int(grainBytes - in)
		}
		if err := s.readGrain(p[done:done+n], uint64(g), in); err != nil {
			return done, err
		}
		done += n
	}
	return done, nil
}

func (s *sparseExtent) readGrain(p []byte, g uint64, in int64) error {
	n := uint64(s.h.NumGTEsPerGT)
	t, err := s.gt(uint32(g / n))
	if err != nil {
		return err
	}
	var ge uint32
	if t != nil {
		ge = t[g%n]
	}
	grainBytes := int64(s.h.GrainSize) * sector
	switch {
	case ge == 0:
		if s.parent != nil {
			_, err := s.parent.ReadAt(p, s.base+int64(g)*grainBytes+in)
			if err == io.EOF {
				err = nil
			}
			return err
		}
		clear(p)
		return nil
	case ge == 1:
		clear(p)
		return nil
	}
	if s.h.Flags&flagCompressed == 0 {
		m, err := s.f.ReadAt(p, int64(ge)*sector+in)
		if m < len(p) {
			clear(p[m:])
			if err != io.EOF && err != nil {
				return err
			}
		}
		return nil
	}
	data, err := s.compressedGrain(ge, grainBytes)
	if err != nil {
		return fmt.Errorf("%s: grain %d: %w", s.name, g, err)
	}
	copy(p, data[in:])
	return nil
}

func (s *sparseExtent) compressedGrain(ge uint32, grainBytes int64) ([]byte, error) {
	if d := s.cache.get(ge); d != nil {
		return d, nil
	}
	hdr := make([]byte, 12)
	if _, err := s.f.ReadAt(hdr, int64(ge)*sector); err != nil {
		return nil, err
	}
	size := binary.LittleEndian.Uint32(hdr[8:])
	if size == 0 || size > uint32(grainBytes)*2+4096 {
		return nil, fmt.Errorf("bad compressed size %d", size)
	}
	comp := make([]byte, size)
	if _, err := s.f.ReadAt(comp, int64(ge)*sector+12); err != nil && err != io.EOF {
		return nil, err
	}
	zr, err := zlib.NewReader(bytes.NewReader(comp))
	if err != nil {
		return nil, err
	}
	out := make([]byte, grainBytes)
	m, err := io.ReadFull(zr, out)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return nil, err
	}
	clear(out[m:])
	s.cache.put(ge, out)
	return out, nil
}

// grainCache is a tiny FIFO cache of decompressed grains.
type grainCache struct {
	mu    sync.Mutex
	max   int
	order []uint32
	m     map[uint32][]byte
}

func newGrainCache(n int) *grainCache { return &grainCache{max: n, m: map[uint32][]byte{}} }

func (c *grainCache) get(k uint32) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.m[k]
}

func (c *grainCache) put(k uint32, v []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.m[k]; ok {
		return
	}
	if len(c.order) >= c.max {
		delete(c.m, c.order[0])
		c.order = c.order[1:]
	}
	c.order = append(c.order, k)
	c.m[k] = v
}
