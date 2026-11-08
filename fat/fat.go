// Package fat reads FAT12, FAT16 and FAT32 filesystems with VFAT long names (read-only).
package fat

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"paleontologist/vfs"
)

// Node is a FAT directory entry (FAT has no inodes, so the entry itself identifies the file).
type Node struct {
	Cluster uint32
	Size    uint32
	Dir     bool
	Mtime   int64 // unix seconds (local time interpreted as UTC wall clock)
	root    bool
}

// FS is an opened FAT filesystem.
type FS struct {
	dev         io.ReaderAt
	bits        int // 12, 16 or 32
	sector      int64
	clusterSize int64
	dataStart   int64 // byte offset of cluster 2
	rootStart   int64 // FAT12/16 fixed root directory
	rootSize    int64
	rootCluster uint32
	clusters    uint32 // number of data clusters
	fat         []byte
	label       string
	serial      uint32

	mu   sync.Mutex
	dirs *vfs.Cache[uint32, []vfs.Entry]
}

// Open parses the boot sector and loads the first FAT.
func Open(dev io.ReaderAt) (*FS, error) {
	b := make([]byte, 512)
	if err := vfs.ReadFull(dev, b, 0); err != nil {
		return nil, fmt.Errorf("fat: reading boot sector: %w", err)
	}
	le := binary.LittleEndian
	f := &FS{dev: dev}
	f.sector = int64(le.Uint16(b[0x0B:]))
	spc := int64(b[0x0D])
	reserved := int64(le.Uint16(b[0x0E:]))
	nfats := int64(b[0x10])
	rootEnts := int64(le.Uint16(b[0x11:]))
	total := int64(le.Uint16(b[0x13:]))
	if total == 0 {
		total = int64(le.Uint32(b[0x20:]))
	}
	fatSz := int64(le.Uint16(b[0x16:]))
	if fatSz == 0 {
		fatSz = int64(le.Uint32(b[0x24:]))
	}
	switch f.sector {
	case 512, 1024, 2048, 4096:
	default:
		return nil, fmt.Errorf("fat: bad sector size %d", f.sector)
	}
	if spc == 0 || spc&(spc-1) != 0 || nfats == 0 || fatSz == 0 || reserved == 0 {
		return nil, errors.New("fat: corrupt boot sector")
	}
	f.clusterSize = spc * f.sector
	rootSecs := (rootEnts*32 + f.sector - 1) / f.sector
	fatStart := reserved * f.sector
	activeFAT := int64(0)
	f.rootStart = (reserved + nfats*fatSz) * f.sector
	f.rootSize = rootEnts * 32
	dataSec := reserved + nfats*fatSz + rootSecs
	f.dataStart = dataSec * f.sector
	if total <= dataSec {
		return nil, errors.New("fat: corrupt boot sector (no data area)")
	}
	f.clusters = uint32((total - dataSec) / spc)
	switch {
	case f.clusters < 4085:
		f.bits = 12
	case f.clusters < 65525:
		f.bits = 16
	default:
		f.bits = 32
	}
	if rootEnts == 0 && f.bits != 32 {
		f.bits = 32 // FAT32 with few clusters (non-standard but seen in the wild)
	}
	if f.bits == 32 {
		f.rootCluster = le.Uint32(b[0x2C:])
		if flags := le.Uint16(b[0x28:]); flags&0x80 != 0 {
			activeFAT = int64(flags & 0xF)
		}
		f.serial = le.Uint32(b[0x43:])
		if string(b[0x52:0x57]) == "FAT32" || b[0x42] == 0x29 {
			f.label = strings.TrimRight(string(b[0x47:0x52]), " \x00")
		}
	} else if b[0x26] == 0x29 {
		f.serial = le.Uint32(b[0x27:])
		f.label = strings.TrimRight(string(b[0x2B:0x36]), " \x00")
	}
	if f.label == "NO NAME" {
		f.label = ""
	}
	need := (int64(f.clusters) + 2) * int64(f.bits) / 8
	if need+2 > fatSz*f.sector {
		need = fatSz*f.sector - 2
	}
	f.fat = make([]byte, need+2)
	if err := vfs.ReadFull(dev, f.fat, fatStart+activeFAT*fatSz*f.sector); err != nil {
		return nil, fmt.Errorf("fat: reading FAT: %w", err)
	}
	f.dirs = vfs.NewCache[uint32, []vfs.Entry](4096)
	// the volume label in the root directory wins over the boot sector copy
	if raw, err := f.readDirRaw(f.rootNode()); err == nil {
		for p := 0; p+32 <= len(raw); p += 32 {
			e := raw[p:]
			if e[0] == 0 {
				break
			}
			if e[0] != 0xE5 && e[11]&0x0F == 0x08 {
				f.label = strings.TrimRight(oemString(e[:11]), " ")
				break
			}
		}
	}
	return f, nil
}

func (f *FS) rootNode() Node { return Node{Cluster: f.rootCluster, Dir: true, root: true} }

func (f *FS) next(c uint32) uint32 {
	switch f.bits {
	case 12:
		o := int(c) + int(c)/2
		if o+1 >= len(f.fat) {
			return 0xFFFFFFFF
		}
		v := uint32(binary.LittleEndian.Uint16(f.fat[o:]))
		if c&1 != 0 {
			v >>= 4
		} else {
			v &= 0xFFF
		}
		if v >= 0xFF7 {
			return 0xFFFFFFFF
		}
		return v
	case 16:
		o := int(c) * 2
		if o+2 > len(f.fat) {
			return 0xFFFFFFFF
		}
		v := uint32(binary.LittleEndian.Uint16(f.fat[o:]))
		if v >= 0xFFF7 {
			return 0xFFFFFFFF
		}
		return v
	}
	o := int(c) * 4
	if o+4 > len(f.fat) {
		return 0xFFFFFFFF
	}
	v := binary.LittleEndian.Uint32(f.fat[o:]) & 0x0FFFFFFF
	if v >= 0x0FFFFFF7 {
		return 0xFFFFFFFF
	}
	return v
}

// chain returns the clusters of a file as runs of consecutive clusters.
type crun struct{ first, n uint32 }

func (f *FS) chain(start uint32, limit uint32) ([]crun, error) {
	var out []crun
	c := start
	count := uint32(0)
	for c >= 2 && c < f.clusters+2 {
		if count > f.clusters || count >= limit {
			break
		}
		if k := len(out); k > 0 && out[k-1].first+out[k-1].n == c {
			out[k-1].n++
		} else {
			out = append(out, crun{c, 1})
		}
		count++
		c = f.next(c)
	}
	if count > f.clusters {
		return nil, errors.New("fat: cluster chain loop")
	}
	return out, nil
}

func (f *FS) clusterOff(c uint32) int64 { return f.dataStart + int64(c-2)*f.clusterSize }

func (f *FS) readDirRaw(n Node) ([]byte, error) {
	if n.root && f.bits != 32 {
		b := make([]byte, f.rootSize)
		return b, vfs.ReadFull(f.dev, b, f.rootStart)
	}
	runs, err := f.chain(n.Cluster, 1<<21/uint32(f.clusterSize)+1) // directories are at most 2 MiB
	if err != nil {
		return nil, err
	}
	var out []byte
	for _, r := range runs {
		b := make([]byte, int64(r.n)*f.clusterSize)
		if err := vfs.ReadFull(f.dev, b, f.clusterOff(r.first)); err != nil {
			return nil, err
		}
		out = append(out, b...)
	}
	return out, nil
}

func fatTime(date, tm uint16) int64 {
	if date == 0 {
		return 315532800 // 1980-01-01
	}
	t := time.Date(1980+int(date>>9), time.Month(date>>5&0xF), int(date&0x1F),
		int(tm>>11), int(tm>>5&0x3F), int(tm&0x1F)*2, 0, time.Local)
	return t.Unix()
}

func lfnChecksum(n []byte) byte {
	var s byte
	for _, c := range n[:11] {
		s = (s>>1 | s<<7) + c
	}
	return s
}

func (f *FS) ReadDir(n vfs.Node) ([]vfs.Entry, error) {
	d := n.(Node)
	if !d.Dir {
		return nil, errors.New("fat: not a directory")
	}
	key := d.Cluster
	f.mu.Lock()
	if e, ok := f.dirs.Get(key); ok {
		f.mu.Unlock()
		return e, nil
	}
	f.mu.Unlock()
	raw, err := f.readDirRaw(d)
	if err != nil {
		return nil, err
	}
	le := binary.LittleEndian
	var out []vfs.Entry
	var lfn []uint16
	var lfnSum byte
	lfnNext := 0
	seen := map[string]bool{}
	for p := 0; p+32 <= len(raw); p += 32 {
		e := raw[p : p+32]
		if e[0] == 0 {
			break
		}
		if e[0] == 0xE5 {
			lfn = nil
			continue
		}
		attr := e[11]
		if attr&0x3F == 0x0F { // long name piece
			ord := int(e[0] & 0x1F)
			if e[0]&0x40 != 0 {
				lfn = make([]uint16, ord*13)
				lfnSum = e[13]
				lfnNext = ord
			}
			if lfn == nil || ord != lfnNext || e[13] != lfnSum || ord == 0 {
				lfn = nil
				continue
			}
			base := (ord - 1) * 13
			for i, off := range []int{1, 3, 5, 7, 9, 14, 16, 18, 20, 22, 24, 28, 30} {
				lfn[base+i] = le.Uint16(e[off:])
			}
			lfnNext--
			continue
		}
		if attr&0x08 != 0 { // volume label
			lfn = nil
			continue
		}
		name := ""
		if lfn != nil && lfnNext == 0 && lfnChecksum(e) == lfnSum {
			end := len(lfn)
			for i, c := range lfn {
				if c == 0 {
					end = i
					break
				}
			}
			name = string(utf16.Decode(lfn[:end]))
		}
		lfn = nil
		if name == "" {
			name = shortName(e)
		}
		if name == "." || name == ".." || name == "" {
			continue
		}
		cl := uint32(le.Uint16(e[26:]))
		if f.bits == 32 {
			cl |= uint32(le.Uint16(e[20:])) << 16
		}
		node := Node{Cluster: cl, Size: le.Uint32(e[28:]), Dir: attr&0x10 != 0,
			Mtime: fatTime(le.Uint16(e[24:]), le.Uint16(e[22:]))}
		k := vfs.KindFile
		if node.Dir {
			k = vfs.KindDir
			node.Size = 0
			if cl == 0 {
				continue // broken directory entry (".." style pointer to root)
			}
		}
		if lk := strings.ToLower(name); seen[lk] {
			continue
		} else {
			seen[lk] = true
		}
		out = append(out, vfs.Entry{Name: name, Node: node, Kind: k})
	}
	vfs.SortEntries(out)
	f.mu.Lock()
	f.dirs.Put(key, out)
	f.mu.Unlock()
	return out, nil
}

func shortName(e []byte) string {
	b := append([]byte(nil), e[:11]...)
	if b[0] == 0x05 {
		b[0] = 0xE5
	}
	base := strings.TrimRight(oemString(b[:8]), " ")
	ext := strings.TrimRight(oemString(b[8:11]), " ")
	// Windows NT stores "all lower case" hints for 8.3 names
	if e[12]&0x08 != 0 {
		base = strings.ToLower(base)
	}
	if e[12]&0x10 != 0 {
		ext = strings.ToLower(ext)
	}
	if ext != "" {
		return base + "." + ext
	}
	return base
}

// cp437 upper half, used for 8.3 names.
const cp437 = "ÇüéâäàåçêëèïîìÄÅÉæÆôöòûùÿÖÜ¢£¥₧ƒáíóúñÑªº¿⌐¬½¼¡«»░▒▓│┤╡╢╖╕╣║╗╝╜╛┐└┴┬├─┼╞╟╚╔╩╦╠═╬╧╨╤╥╙╘╒╓╫╪┘┌█▄▌▐▀αßΓπΣσµτΦΘΩδ∞φε∩≡±≥≤⌠⌡÷≈°∙·√ⁿ²■ "

var cp437Runes = []rune(cp437)

func oemString(b []byte) string {
	r := make([]rune, len(b))
	for i, c := range b {
		if c < 0x80 || len(cp437Runes) != 128 {
			r[i] = rune(c)
		} else {
			r[i] = cp437Runes[c-0x80]
		}
	}
	return string(r)
}

// --- vfs.FS ---

func (f *FS) Type() string          { return fmt.Sprintf("fat%d", f.bits) }
func (f *FS) Label() string         { return f.label }
func (f *FS) Root() vfs.Node        { return f.rootNode() }
func (f *FS) CaseInsensitive() bool { return true }

func (f *FS) Stat(n vfs.Node) (vfs.Stat, error) {
	d := n.(Node)
	st := vfs.Stat{Size: int64(d.Size), Mode: 0o644, Mtime: time.Unix(d.Mtime, 0)}
	if d.Dir {
		st.Mode = fs.ModeDir | 0o755
		st.Size = 0
	}
	return st, nil
}

func (f *FS) Readlink(vfs.Node) (string, error) { return "", errors.New("fat: no symlinks") }

// File is an open FAT file.
type File struct {
	fs     *FS
	size   int64
	runs   []crun
	starts []int64 // file offset of each run
}

func (fl *File) Size() int64 { return fl.size }

func (f *FS) Open(n vfs.Node) (vfs.File, error) {
	d := n.(Node)
	if d.Dir {
		return nil, errors.New("fat: is a directory")
	}
	need := uint32((int64(d.Size) + f.clusterSize - 1) / f.clusterSize)
	runs, err := f.chain(d.Cluster, need)
	if err != nil {
		return nil, err
	}
	fl := &File{fs: f, size: int64(d.Size), runs: runs, starts: make([]int64, len(runs))}
	var pos int64
	for i, r := range runs {
		fl.starts[i] = pos
		pos += int64(r.n) * f.clusterSize
	}
	return fl, nil
}

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
	cs := fl.fs.clusterSize
	done := 0
	starts := fl.starts
	for done < want {
		cur := off + int64(done)
		ri := sort.Search(len(starts), func(i int) bool { return starts[i] > cur }) - 1
		if ri < 0 || cur >= starts[ri]+int64(fl.runs[ri].n)*cs {
			return done, errors.New("fat: cluster chain shorter than file size")
		}
		r := fl.runs[ri]
		in := cur - starts[ri]
		n := int64(want - done)
		if rem := int64(r.n)*cs - in; n > rem {
			n = rem
		}
		if err := vfs.ReadFull(fl.fs.dev, p[done:done+int(n)], fl.fs.clusterOff(r.first)+in); err != nil {
			return done, err
		}
		done += int(n)
	}
	return done, eof
}

func (f *FS) Info() []string {
	return []string{
		fmt.Sprintf("serial: %04X-%04X", f.serial>>16, f.serial&0xFFFF),
		fmt.Sprintf("size:  %.1f GiB   cluster size: %d", float64(f.clusters)*float64(f.clusterSize)/(1<<30), f.clusterSize),
	}
}
