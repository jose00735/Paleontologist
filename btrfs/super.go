// Package btrfs is a read-only btrfs filesystem reader written in pure Go.
//
// It understands single-device filesystems (and multi-device ones whose data is
// mirrored, e.g. RAID1/DUP), every subvolume and snapshot, inline and regular
// extents, holes, preallocated extents and zlib / lzo / zstd compression.
package btrfs

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

const (
	superOffset = 0x10000
	magic       = "_BHRfS_M"
)

// Item types.
const (
	typeInodeItem  = 1
	typeInodeRef   = 12
	typeDirItem    = 84
	typeDirIndex   = 96
	typeExtentData = 108
	typeRootItem   = 132
	typeRootRef    = 156
	typeChunkItem  = 228
)

// Well-known object ids.
const (
	RootTreeID       = 1
	ChunkTreeID      = 3
	FSTreeID         = 5
	FirstFreeID      = 256
	firstChunkTreeID = 256
)

// Block group profile bits.
const (
	bgRAID0   = 1 << 3
	bgRAID1   = 1 << 4
	bgDUP     = 1 << 5
	bgRAID10  = 1 << 6
	bgRAID5   = 1 << 7
	bgRAID6   = 1 << 8
	bgRAID1C3 = 1 << 9
	bgRAID1C4 = 1 << 10
)

// Super holds the fields of the superblock we need.
type Super struct {
	FSID           [16]byte
	Generation     uint64
	Root           uint64
	ChunkRoot      uint64
	TotalBytes     uint64
	BytesUsed      uint64
	NumDevices     uint64
	SectorSize     uint32
	NodeSize       uint32
	RootLevel      uint8
	ChunkRootLevel uint8
	DevID          uint64
	Label          string
	sysChunks      []byte
}

// UUID returns the filesystem UUID as text.
func (s *Super) UUID() string {
	h := hex.EncodeToString(s.FSID[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

type stripe struct {
	devID  uint64
	offset uint64
}

type chunk struct {
	logical   uint64
	length    uint64
	stripeLen uint64
	typ       uint64
	subStripe uint16
	stripes   []stripe
}

func parseSuper(b []byte) (*Super, error) {
	if string(b[0x40:0x48]) != magic {
		return nil, errors.New("btrfs: superblock magic not found")
	}
	le := binary.LittleEndian
	s := &Super{}
	copy(s.FSID[:], b[0x20:0x30])
	s.Generation = le.Uint64(b[0x48:])
	s.Root = le.Uint64(b[0x50:])
	s.ChunkRoot = le.Uint64(b[0x58:])
	s.TotalBytes = le.Uint64(b[0x70:])
	s.BytesUsed = le.Uint64(b[0x78:])
	s.NumDevices = le.Uint64(b[0x88:])
	s.SectorSize = le.Uint32(b[0x90:])
	s.NodeSize = le.Uint32(b[0x94:])
	sysLen := le.Uint32(b[0xa0:])
	s.RootLevel = b[0xc6]
	s.ChunkRootLevel = b[0xc7]
	s.DevID = le.Uint64(b[0xc9:])
	s.Label = strings.TrimRight(string(b[0x12b:0x12b+256]), "\x00")
	if sysLen > 2048 {
		return nil, errors.New("btrfs: corrupt sys_chunk_array")
	}
	s.sysChunks = append([]byte(nil), b[0x32b:0x32b+int(sysLen)]...)
	if s.NodeSize < 4096 || s.NodeSize > 65536 || s.SectorSize < 512 || s.SectorSize > 65536 {
		return nil, fmt.Errorf("btrfs: unsupported node/sector size %d/%d", s.NodeSize, s.SectorSize)
	}
	return s, nil
}

// parseChunk decodes a btrfs_chunk structure. It returns the chunk and its encoded size.
func parseChunk(logical uint64, b []byte) (chunk, int, error) {
	le := binary.LittleEndian
	if len(b) < 48 {
		return chunk{}, 0, errors.New("btrfs: short chunk item")
	}
	c := chunk{logical: logical, length: le.Uint64(b[0:]), stripeLen: le.Uint64(b[16:]), typ: le.Uint64(b[24:])}
	n := int(le.Uint16(b[44:]))
	c.subStripe = le.Uint16(b[46:])
	size := 48 + n*32
	if n == 0 || len(b) < size {
		return chunk{}, 0, errors.New("btrfs: bad chunk stripes")
	}
	for i := 0; i < n; i++ {
		s := b[48+i*32:]
		c.stripes = append(c.stripes, stripe{devID: le.Uint64(s[0:]), offset: le.Uint64(s[8:])})
	}
	return c, size, nil
}

// mapper translates logical addresses to physical offsets on our device.
type mapper struct {
	chunks []chunk
	devID  uint64
}

func (m *mapper) add(c chunk) {
	i := sort.Search(len(m.chunks), func(i int) bool { return m.chunks[i].logical >= c.logical })
	if i < len(m.chunks) && m.chunks[i].logical == c.logical {
		m.chunks[i] = c
		return
	}
	m.chunks = append(m.chunks, chunk{})
	copy(m.chunks[i+1:], m.chunks[i:])
	m.chunks[i] = c
}

// resolve returns the physical offset of logical and how many bytes are contiguous from there.
func (m *mapper) resolve(logical uint64) (phys uint64, contig uint64, err error) {
	i := sort.Search(len(m.chunks), func(i int) bool { return m.chunks[i].logical+m.chunks[i].length > logical })
	if i == len(m.chunks) || m.chunks[i].logical > logical {
		return 0, 0, fmt.Errorf("btrfs: logical address %#x not mapped by any chunk", logical)
	}
	c := &m.chunks[i]
	off := logical - c.logical
	mine := func(s stripe) bool { return s.devID == m.devID }
	switch {
	case c.typ&(bgRAID5|bgRAID6) != 0:
		return 0, 0, errors.New("btrfs: RAID5/RAID6 profiles are not supported")
	case c.typ&bgRAID0 != 0:
		sl := c.stripeLen
		nr := off / sl
		idx := nr % uint64(len(c.stripes))
		s := c.stripes[idx]
		if !mine(s) {
			return 0, 0, errors.New("btrfs: RAID0 data lives on another device that is not available")
		}
		return s.offset + (nr/uint64(len(c.stripes)))*sl + off%sl, sl - off%sl, nil
	case c.typ&bgRAID10 != 0:
		sub := uint64(c.subStripe)
		if sub == 0 {
			sub = 2
		}
		sl := c.stripeLen
		factor := uint64(len(c.stripes)) / sub
		nr := off / sl
		base := (nr % factor) * sub
		for k := uint64(0); k < sub; k++ {
			if s := c.stripes[base+k]; mine(s) {
				return s.offset + (nr/factor)*sl + off%sl, sl - off%sl, nil
			}
		}
		return 0, 0, errors.New("btrfs: RAID10 data lives on another device that is not available")
	default:
		// SINGLE, DUP, RAID1, RAID1C3, RAID1C4: every stripe holds a full copy.
		for _, s := range c.stripes {
			if mine(s) {
				return s.offset + off, c.length - off, nil
			}
		}
		return 0, 0, errors.New("btrfs: this chunk lives on another device of a multi-device filesystem")
	}
}

// readLogical reads len(p) bytes at a logical address.
func (fs *FS) readLogical(p []byte, logical uint64) error {
	done := 0
	for done < len(p) {
		phys, contig, err := fs.m.resolve(logical + uint64(done))
		if err != nil {
			return err
		}
		n := len(p) - done
		if uint64(n) > contig {
			n = int(contig)
		}
		m, err := fs.dev.ReadAt(p[done:done+n], int64(phys))
		if m < n {
			if err == nil {
				err = io.ErrUnexpectedEOF
			}
			return fmt.Errorf("btrfs: reading %#x: %w", logical, err)
		}
		done += n
	}
	return nil
}
