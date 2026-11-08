// Package ext reads ext2, ext3 and ext4 filesystems (read-only, pure Go).
package ext

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"sync"
	"time"

	"paleontologist/vfs"
)

const (
	compatHasJournal = 0x4
	compatSparse2    = 0x200
	incompatFiletype = 0x2
	incompatRecover  = 0x4
	incompatJournalD = 0x8
	incompatMetaBG   = 0x10
	incompatExtents  = 0x40
	incompat64bit    = 0x80
	incompatFlexBG   = 0x200
	incompatInline   = 0x8000
	incompatEncrypt  = 0x10000
	roSparseSuper    = 0x1
	roHugeFile       = 0x8
	flagIndex        = 0x1000
	flagHugeFile     = 0x40000
	flagExtents      = 0x80000
	flagInlineData   = 0x10000000
	flagEncrypt      = 0x800
	flagCompr        = 0x4
	extentMagic      = 0xF30A
	rootIno          = 2
)

// FS is an opened ext2/3/4 filesystem.
type FS struct {
	dev        io.ReaderAt
	bs         int64 // block size
	inodeSize  int64
	ipg        uint32 // inodes per group
	bpg        uint32 // blocks per group
	firstData  uint32
	groups     uint32
	blocks     uint64
	descSize   int64
	compat     uint32
	incompat   uint32
	roCompat   uint32
	label      string
	uuid       [16]byte
	tables     []uint64 // inode table block per group
	freeBlocks uint64
	mounted    time.Time

	mu      sync.Mutex
	dirs    *vfs.Cache[uint32, []vfs.Entry]
	files   *vfs.Cache[uint32, *File]
	parents *vfs.Cache[uint32, uint32]
}

// Open reads the superblock and group descriptors.
func Open(dev io.ReaderAt) (*FS, error) {
	sb := make([]byte, 1024)
	if err := vfs.ReadFull(dev, sb, 1024); err != nil {
		return nil, fmt.Errorf("ext: reading superblock: %w", err)
	}
	le := binary.LittleEndian
	if le.Uint16(sb[0x38:]) != 0xEF53 {
		return nil, errors.New("ext: bad superblock magic")
	}
	f := &FS{dev: dev}
	logBS := le.Uint32(sb[0x18:])
	if logBS > 6 {
		return nil, fmt.Errorf("ext: unsupported block size 2^%d KiB", logBS)
	}
	f.bs = 1024 << logBS
	f.firstData = le.Uint32(sb[0x14:])
	f.bpg = le.Uint32(sb[0x20:])
	f.ipg = le.Uint32(sb[0x28:])
	f.mounted = time.Unix(int64(le.Uint32(sb[0x2C:])), 0)
	f.inodeSize = 128
	if le.Uint32(sb[0x4C:]) >= 1 {
		f.inodeSize = int64(le.Uint16(sb[0x58:]))
		f.compat = le.Uint32(sb[0x5C:])
		f.incompat = le.Uint32(sb[0x60:])
		f.roCompat = le.Uint32(sb[0x64:])
	}
	copy(f.uuid[:], sb[0x68:])
	f.label = strings.TrimRight(string(sb[0x78:0x88]), "\x00")
	f.blocks = uint64(le.Uint32(sb[0x04:]))
	f.freeBlocks = uint64(le.Uint32(sb[0x0C:]))
	f.descSize = 32
	if f.incompat&incompat64bit != 0 {
		f.blocks |= uint64(le.Uint32(sb[0x150:])) << 32
		f.freeBlocks |= uint64(le.Uint32(sb[0x158:])) << 32
		if ds := int64(le.Uint16(sb[0xFE:])); ds >= 64 {
			f.descSize = ds
		}
	}
	if f.incompat&incompatJournalD != 0 {
		return nil, errors.New("ext: this is an external journal device, not a filesystem")
	}
	if f.bpg == 0 || f.ipg == 0 || f.inodeSize < 128 || f.inodeSize > f.bs {
		return nil, errors.New("ext: corrupt superblock")
	}
	f.groups = uint32((f.blocks - uint64(f.firstData) + uint64(f.bpg) - 1) / uint64(f.bpg))
	if err := f.readGroups(le.Uint32(sb[0x104:])); err != nil {
		return nil, err
	}
	f.dirs = vfs.NewCache[uint32, []vfs.Entry](4096)
	f.files = vfs.NewCache[uint32, *File](64)
	f.parents = vfs.NewCache[uint32, uint32](8192)
	return f, nil
}

func (f *FS) hasSuper(g uint32) bool {
	if g <= 1 {
		return true
	}
	if f.roCompat&roSparseSuper == 0 {
		return true
	}
	for _, b := range []uint32{3, 5, 7} {
		n := b
		for n < g {
			n *= b
		}
		if n == g {
			return true
		}
	}
	return false
}

func (f *FS) groupFirstBlock(g uint32) uint64 {
	return uint64(f.firstData) + uint64(g)*uint64(f.bpg)
}

func (f *FS) readGroups(firstMetaBG uint32) error {
	perBlock := uint32(f.bs / f.descSize)
	f.tables = make([]uint64, f.groups)
	le := binary.LittleEndian
	blk := make([]byte, f.bs)
	lastBlock := uint64(1<<64 - 1)
	for g := uint32(0); g < f.groups; g++ {
		var b uint64
		idx := g / perBlock
		if f.incompat&incompatMetaBG == 0 || idx < firstMetaBG {
			b = uint64(f.firstData) + 1 + uint64(idx)
		} else {
			first := idx * perBlock
			b = f.groupFirstBlock(first)
			if f.hasSuper(first) {
				b++
			}
		}
		if b != lastBlock {
			if err := vfs.ReadFull(f.dev, blk, int64(b)*f.bs); err != nil {
				return fmt.Errorf("ext: reading group descriptors: %w", err)
			}
			lastBlock = b
		}
		d := blk[int64(g%perBlock)*f.descSize:]
		t := uint64(le.Uint32(d[8:]))
		if f.descSize >= 64 {
			t |= uint64(le.Uint32(d[0x28:])) << 32
		}
		f.tables[g] = t
	}
	return nil
}

// inode is a parsed on-disk inode.
type inode struct {
	num    uint32
	mode   uint16
	size   int64
	flags  uint32
	mtime  time.Time
	blocks uint64 // in 512-byte units
	acl    uint64 // xattr block
	iblock []byte // 60 bytes
	raw    []byte
}

func (f *FS) readInode(n uint32) (*inode, error) {
	if n == 0 || (n-1)/f.ipg >= f.groups {
		return nil, fmt.Errorf("ext: bad inode number %d", n)
	}
	g, i := (n-1)/f.ipg, (n-1)%f.ipg
	raw := make([]byte, f.inodeSize)
	if err := vfs.ReadFull(f.dev, raw, int64(f.tables[g])*f.bs+int64(i)*f.inodeSize); err != nil {
		return nil, err
	}
	le := binary.LittleEndian
	in := &inode{num: n, raw: raw, iblock: raw[0x28:0x64]}
	in.mode = le.Uint16(raw)
	in.size = int64(le.Uint32(raw[4:])) | int64(le.Uint32(raw[0x6C:]))<<32
	if in.size < 0 {
		return nil, fmt.Errorf("ext: inode %d is corrupt (bad size)", n)
	}
	in.flags = le.Uint32(raw[0x20:])
	in.blocks = uint64(le.Uint32(raw[0x1C:])) | uint64(le.Uint16(raw[0x74:]))<<32
	if f.roCompat&roHugeFile != 0 && in.flags&flagHugeFile != 0 {
		in.blocks *= uint64(f.bs / 512)
	}
	in.acl = uint64(le.Uint32(raw[0x68:])) | uint64(le.Uint16(raw[0x76:]))<<32
	sec := int64(int32(le.Uint32(raw[0x10:])))
	var nsec int64
	if f.inodeSize > 128 {
		extra := int64(le.Uint16(raw[0x80:]))
		if extra >= 24 && 128+extra <= f.inodeSize {
			x := le.Uint32(raw[0x88:])
			sec += int64(x&3) << 32
			nsec = int64(x >> 2)
		}
	}
	in.mtime = time.Unix(sec, nsec)
	return in, nil
}

func (in *inode) fileMode() fs.FileMode {
	m := fs.FileMode(in.mode & 0o777)
	switch in.mode & 0o170000 {
	case 0o040000:
		m |= fs.ModeDir
	case 0o120000:
		m |= fs.ModeSymlink
	case 0o100000:
	case 0o010000:
		m |= fs.ModeNamedPipe
	case 0o140000:
		m |= fs.ModeSocket
	case 0o020000:
		m |= fs.ModeDevice | fs.ModeCharDevice
	default:
		m |= fs.ModeDevice
	}
	return m
}

// inlineXattrData returns the "system.data" value stored in the inode body (inline_data feature).
func (f *FS) inlineXattrData(in *inode) []byte {
	if f.inodeSize <= 128 {
		return nil
	}
	le := binary.LittleEndian
	extra := int(le.Uint16(in.raw[0x80:]))
	start := 128 + extra
	if start+4 > len(in.raw) || le.Uint32(in.raw[start:]) != 0xEA020000 {
		return nil
	}
	base := start + 4
	p := base
	for p+16 <= len(in.raw) {
		nameLen := int(in.raw[p])
		if nameLen == 0 && le.Uint32(in.raw[p:]) == 0 {
			break
		}
		idx := in.raw[p+1]
		voff := int(le.Uint16(in.raw[p+2:]))
		vsize := int(le.Uint32(in.raw[p+8:]))
		if p+16+nameLen > len(in.raw) {
			break
		}
		name := string(in.raw[p+16 : p+16+nameLen])
		if idx == 7 && name == "data" {
			if base+voff+vsize <= len(in.raw) {
				return in.raw[base+voff : base+voff+vsize]
			}
			return nil
		}
		p += (16 + nameLen + 3) &^ 3
	}
	return nil
}

// --- vfs.FS ---

func (f *FS) Type() string {
	switch {
	case f.incompat&(incompatExtents|incompat64bit|incompatFlexBG) != 0:
		return "ext4"
	case f.compat&compatHasJournal != 0:
		return "ext3"
	}
	return "ext2"
}

func (f *FS) Label() string  { return f.label }
func (f *FS) Root() vfs.Node { return uint32(rootIno) }

func (f *FS) Stat(n vfs.Node) (vfs.Stat, error) {
	in, err := f.readInode(n.(uint32))
	if err != nil {
		return vfs.Stat{}, err
	}
	return vfs.Stat{Size: in.size, Mode: in.fileMode(), Mtime: in.mtime}, nil
}

func (f *FS) ReadDir(n vfs.Node) ([]vfs.Entry, error) {
	num := n.(uint32)
	f.mu.Lock()
	if e, ok := f.dirs.Get(num); ok {
		f.mu.Unlock()
		return e, nil
	}
	f.mu.Unlock()
	in, err := f.readInode(num)
	if err != nil {
		return nil, err
	}
	if in.mode&0o170000 != 0o040000 {
		return nil, errors.New("ext: not a directory")
	}
	if in.flags&flagEncrypt != 0 {
		return nil, errors.New("ext: encrypted directory (fscrypt) can't be read")
	}
	var out []vfs.Entry
	parent := uint32(0)
	if in.flags&flagInlineData != 0 {
		// 4-byte parent inode, then entries in i_block, continued in system.data
		parent = binary.LittleEndian.Uint32(in.iblock)
		out = f.parseDirBlock(in.iblock[4:], out, &parent)
		if x := f.inlineXattrData(in); len(x) > 0 {
			out = f.parseDirBlock(x, out, &parent)
		}
	} else {
		file, err := f.openInode(in)
		if err != nil {
			return nil, err
		}
		buf := make([]byte, f.bs)
		for off := int64(0); off < in.size; off += f.bs {
			n, err := file.ReadAt(buf, off)
			if err != nil && err != io.EOF {
				return nil, err
			}
			out = f.parseDirBlock(buf[:n], out, &parent)
		}
	}
	vfs.SortEntries(out)
	f.mu.Lock()
	f.dirs.Put(num, out)
	if parent != 0 {
		f.parents.Put(num, parent)
	}
	f.mu.Unlock()
	return out, nil
}

func (f *FS) parseDirBlock(b []byte, out []vfs.Entry, parent *uint32) []vfs.Entry {
	le := binary.LittleEndian
	p := 0
	for p+8 <= len(b) {
		ino := le.Uint32(b[p:])
		rl := int(le.Uint16(b[p+4:]))
		if rl == 0 || rl == 65535 {
			rl = int(f.bs) // 64 KiB blocks
			if p != 0 {
				break
			}
		}
		if rl < 8 || p+rl > len(b) {
			break
		}
		nl := int(b[p+6])
		ft := b[p+7]
		if f.incompat&incompatFiletype == 0 {
			nl = int(le.Uint16(b[p+6:]))
			ft = 0
		}
		if ino != 0 && nl > 0 && 8+nl <= rl {
			name := string(b[p+8 : p+8+nl])
			if name == ".." {
				*parent = ino
			}
			if name != "." && name != ".." {
				k := vfs.KindUnknown
				switch ft {
				case 1:
					k = vfs.KindFile
				case 2:
					k = vfs.KindDir
				case 7:
					k = vfs.KindSymlink
				case 3, 4, 5, 6:
					k = vfs.KindOther
				}
				out = append(out, vfs.Entry{Name: name, Node: ino, Kind: k})
			}
		}
		p += rl
	}
	return out
}

// Parent returns the directory that holds directory n (from its ".." entry).
func (f *FS) Parent(n vfs.Node) (vfs.Node, error) {
	num := n.(uint32)
	f.mu.Lock()
	p, ok := f.parents.Get(num)
	f.mu.Unlock()
	if !ok {
		if _, err := f.ReadDir(n); err != nil {
			return nil, err
		}
		f.mu.Lock()
		p, ok = f.parents.Get(num)
		f.mu.Unlock()
		if !ok {
			return uint32(rootIno), nil
		}
	}
	return p, nil
}

func (f *FS) Open(n vfs.Node) (vfs.File, error) {
	num := n.(uint32)
	f.mu.Lock()
	if fl, ok := f.files.Get(num); ok {
		f.mu.Unlock()
		return fl, nil
	}
	f.mu.Unlock()
	in, err := f.readInode(num)
	if err != nil {
		return nil, err
	}
	fl, err := f.openInode(in)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.files.Put(num, fl)
	f.mu.Unlock()
	return fl, nil
}

func (f *FS) Readlink(n vfs.Node) (string, error) {
	in, err := f.readInode(n.(uint32))
	if err != nil {
		return "", err
	}
	if in.size > 4096 {
		return "", errors.New("ext: symlink target too long")
	}
	if in.flags&(flagInlineData|flagExtents) == 0 && in.size < 60 {
		ea := uint64(0)
		if in.acl != 0 {
			ea = uint64(f.bs / 512)
		}
		if in.blocks <= ea {
			return string(in.iblock[:in.size]), nil // fast symlink
		}
	}
	fl, err := f.openInode(in)
	if err != nil {
		return "", err
	}
	b := make([]byte, in.size)
	k, err := fl.ReadAt(b, 0)
	if err != nil && err != io.EOF {
		return "", err
	}
	return string(b[:k]), nil
}

func (f *FS) Info() []string {
	u := f.uuid
	out := []string{
		fmt.Sprintf("uuid:  %x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16]),
		fmt.Sprintf("size:  %.1f GiB (%.1f GiB used)   block size: %d   inode size: %d",
			float64(f.blocks)*float64(f.bs)/(1<<30), float64(f.blocks-f.freeBlocks)*float64(f.bs)/(1<<30), f.bs, f.inodeSize),
	}
	if f.incompat&incompatRecover != 0 {
		out = append(out, "note: the journal was not cleanly closed (VM not shut down?); the most recent changes may be missing")
	}
	return out
}
