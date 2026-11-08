// Package ntfs reads NTFS filesystems (read-only, pure Go): MFT records with fixups,
// attribute lists, fragmented/sparse/LZNT1-compressed data, B-tree directory indexes,
// symbolic links and junctions.
package ntfs

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

const (
	atStdInfo    = 0x10
	atAttrList   = 0x20
	atFileName   = 0x30
	atVolName    = 0x60
	atData       = 0x80
	atIndexRoot  = 0x90
	atIndexAlloc = 0xA0
	atReparse    = 0xC0
	atEnd        = 0xFFFFFFFF

	flagCompressed = 0x0001
	flagEncrypted  = 0x4000
	flagSparse     = 0x8000

	tagSymlink    = 0xA000000C
	tagMountPoint = 0xA0000003
	tagWOF        = 0x80000017
	tagDedup      = 0x80000013

	rootRecord = 5
	refMask    = 0x0000FFFFFFFFFFFF
)

// FS is an opened NTFS volume.
type FS struct {
	dev         io.ReaderAt
	sector      int64
	clusterSize int64
	recordSize  int64
	indexSize   int64
	totalBytes  int64
	serial      uint64
	mft         *stream // $MFT's own data
	label       string
	version     string

	mu   sync.Mutex
	recs *vfs.Cache[uint64, *record]
	dirs *vfs.Cache[uint64, []vfs.Entry]
}

// nrun is a run of clusters of a non-resident attribute.
type nrun struct {
	vcn, lcn, n uint64
	sparse      bool
}

type attr struct {
	typ      uint32
	name     string
	nonres   bool
	flags    uint16
	value    []byte // resident value
	startVCN uint64
	runs     []nrun
	alloc    int64
	size     int64
	initSize int64
	cu       uint8 // log2 of compression unit in clusters
}

type record struct {
	num   uint64
	flags uint16
	attrs []attr
}

func (r *record) isDir() bool { return r.flags&0x2 != 0 }

func (r *record) find(typ uint32, name string) *attr {
	for i := range r.attrs {
		if r.attrs[i].typ == typ && r.attrs[i].name == name {
			return &r.attrs[i]
		}
	}
	return nil
}

// Open parses the boot sector and $MFT.
func Open(dev io.ReaderAt) (*FS, error) {
	b := make([]byte, 512)
	if err := vfs.ReadFull(dev, b, 0); err != nil {
		return nil, fmt.Errorf("ntfs: reading boot sector: %w", err)
	}
	if string(b[3:11]) != "NTFS    " {
		return nil, errors.New("ntfs: not an NTFS boot sector")
	}
	le := binary.LittleEndian
	f := &FS{dev: dev}
	f.sector = int64(le.Uint16(b[0x0B:]))
	spc := int64(b[0x0D])
	if spc > 0x80 {
		spc = 1 << (256 - spc)
	}
	if f.sector < 256 || f.sector > 4096 || spc == 0 {
		return nil, errors.New("ntfs: corrupt boot sector")
	}
	f.clusterSize = f.sector * spc
	f.totalBytes = int64(le.Uint64(b[0x28:])) * f.sector
	sizeOf := func(v int8) int64 {
		if v < 0 {
			return 1 << uint(-v)
		}
		return int64(v) * f.clusterSize
	}
	f.recordSize = sizeOf(int8(b[0x40]))
	f.indexSize = sizeOf(int8(b[0x44]))
	f.serial = le.Uint64(b[0x48:])
	if f.recordSize < 512 || f.recordSize > 65536 {
		return nil, errors.New("ntfs: bad MFT record size")
	}
	mftLCN := int64(le.Uint64(b[0x30:]))
	f.recs = vfs.NewCache[uint64, *record](2048)
	f.dirs = vfs.NewCache[uint64, []vfs.Entry](4096)

	// bootstrap: record 0 sits at the start of the MFT
	raw := make([]byte, f.recordSize)
	if err := vfs.ReadFull(dev, raw, mftLCN*f.clusterSize); err != nil {
		return nil, fmt.Errorf("ntfs: reading $MFT: %w", err)
	}
	if err := f.fixup(raw); err != nil {
		return nil, fmt.Errorf("ntfs: $MFT record: %w", err)
	}
	r0, err := parseRecord(0, raw)
	if err != nil {
		return nil, err
	}
	d := r0.find(atData, "")
	if d == nil || !d.nonres {
		return nil, errors.New("ntfs: $MFT has no data attribute")
	}
	f.mft = &stream{fs: f, a: *d}
	if r0.find(atAttrList, "") != nil {
		// fragmented $MFT: reload with the extension records (found through the first fragment)
		full, err := f.loadRecord(0, raw)
		if err != nil {
			return nil, err
		}
		if d := full.find(atData, ""); d != nil {
			f.mft = &stream{fs: f, a: *d}
		}
	}
	if vr, err := f.rec(3); err == nil {
		if a := vr.find(atVolName, ""); a != nil {
			f.label = utf16le(a.value)
		}
		if a := vr.find(0x70, ""); a != nil && len(a.value) >= 10 {
			f.version = fmt.Sprintf("%d.%d", a.value[8], a.value[9])
		}
	}
	if _, err := f.rec(rootRecord); err != nil {
		return nil, fmt.Errorf("ntfs: root directory: %w", err)
	}
	return f, nil
}

// fixup applies the update sequence array of a multi-sector record.
func (f *FS) fixup(b []byte) error {
	le := binary.LittleEndian
	off := int(le.Uint16(b[4:]))
	cnt := int(le.Uint16(b[6:]))
	if cnt == 0 || off+cnt*2 > len(b) {
		return errors.New("bad update sequence")
	}
	usn := le.Uint16(b[off:])
	for i := 1; i < cnt; i++ {
		end := i*512 - 2
		if end+2 > len(b) {
			break
		}
		if le.Uint16(b[end:]) != usn {
			return errors.New("torn record (update sequence mismatch)")
		}
		copy(b[end:end+2], b[off+i*2:off+i*2+2])
	}
	return nil
}

func utf16le(b []byte) string {
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = binary.LittleEndian.Uint16(b[i*2:])
	}
	return string(utf16.Decode(u))
}

func parseRecord(num uint64, b []byte) (*record, error) {
	le := binary.LittleEndian
	if string(b[:4]) != "FILE" {
		return nil, fmt.Errorf("ntfs: record %d: bad signature", num)
	}
	r := &record{num: num, flags: le.Uint16(b[0x16:])}
	p := int(le.Uint16(b[0x14:]))
	used := int(le.Uint32(b[0x18:]))
	if used > len(b) {
		used = len(b)
	}
	for p+16 <= used {
		typ := le.Uint32(b[p:])
		if typ == atEnd {
			break
		}
		l := int(le.Uint32(b[p+4:]))
		if l < 16 || p+l > used {
			break
		}
		a, err := parseAttr(b[p : p+l])
		if err == nil {
			r.attrs = append(r.attrs, a)
		}
		p += l
	}
	return r, nil
}

func parseAttr(b []byte) (attr, error) {
	le := binary.LittleEndian
	a := attr{typ: le.Uint32(b), nonres: b[8] != 0, flags: le.Uint16(b[0x0C:])}
	if nl := int(b[9]); nl > 0 {
		no := int(le.Uint16(b[0x0A:]))
		if no+nl*2 > len(b) {
			return a, errors.New("bad attribute name")
		}
		a.name = utf16le(b[no : no+nl*2])
	}
	if !a.nonres {
		vl := int(le.Uint32(b[0x10:]))
		vo := int(le.Uint16(b[0x14:]))
		if vo+vl > len(b) {
			return a, errors.New("bad resident value")
		}
		a.value = b[vo : vo+vl]
		a.size = int64(vl)
		a.initSize = a.size
		return a, nil
	}
	if len(b) < 0x40 {
		return a, errors.New("short non-resident attribute")
	}
	a.startVCN = le.Uint64(b[0x10:])
	ro := int(le.Uint16(b[0x20:]))
	a.cu = uint8(le.Uint16(b[0x22:]))
	a.alloc = int64(le.Uint64(b[0x28:]))
	a.size = int64(le.Uint64(b[0x30:]))
	a.initSize = int64(le.Uint64(b[0x38:]))
	if ro > len(b) {
		return a, errors.New("bad runlist offset")
	}
	runs, err := parseRuns(b[ro:], a.startVCN)
	if err != nil {
		return a, err
	}
	a.runs = runs
	return a, nil
}

func parseRuns(b []byte, vcn uint64) ([]nrun, error) {
	var out []nrun
	var lcn int64
	p := 0
	for p < len(b) && b[p] != 0 {
		h := b[p]
		ls, os := int(h&0xF), int(h>>4)
		p++
		if ls == 0 || ls > 8 || os > 8 || p+ls+os > len(b) {
			return nil, errors.New("ntfs: bad runlist")
		}
		var n uint64
		for i := ls - 1; i >= 0; i-- {
			n = n<<8 | uint64(b[p+i])
		}
		p += ls
		r := nrun{vcn: vcn, n: n}
		if os == 0 {
			r.sparse = true
		} else {
			var d int64
			for i := os - 1; i >= 0; i-- {
				d = d<<8 | int64(b[p+i])
			}
			if b[p+os-1]&0x80 != 0 { // sign-extend
				d -= 1 << (8 * uint(os))
			}
			p += os
			lcn += d
			r.lcn = uint64(lcn)
		}
		out = append(out, r)
		vcn += n
	}
	return out, nil
}

// readRaw reads MFT record n (with fixups applied).
func (f *FS) readRaw(n uint64) ([]byte, error) {
	b := make([]byte, f.recordSize)
	k, err := f.mft.ReadAt(b, int64(n)*f.recordSize)
	if k < len(b) {
		if err == nil {
			err = io.ErrUnexpectedEOF
		}
		return nil, fmt.Errorf("ntfs: record %d: %w", n, err)
	}
	if err := f.fixup(b); err != nil {
		return nil, fmt.Errorf("ntfs: record %d: %w", n, err)
	}
	return b, nil
}

// rec returns record n with every attribute (including those in extension records).
func (f *FS) rec(n uint64) (*record, error) {
	f.mu.Lock()
	if r, ok := f.recs.Get(n); ok {
		f.mu.Unlock()
		return r, nil
	}
	f.mu.Unlock()
	raw, err := f.readRaw(n)
	if err != nil {
		return nil, err
	}
	r, err := f.loadRecord(n, raw)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.recs.Put(n, r)
	f.mu.Unlock()
	return r, nil
}

func (f *FS) loadRecord(n uint64, raw []byte) (*record, error) {
	r, err := parseRecord(n, raw)
	if err != nil {
		return nil, err
	}
	if r.flags&0x1 == 0 {
		return nil, fmt.Errorf("ntfs: record %d is not in use", n)
	}
	al := r.find(atAttrList, "")
	if al == nil {
		return r, nil
	}
	// gather attributes from the extension records named in the attribute list
	data := al.value
	if al.nonres {
		data = make([]byte, al.size)
		s := &stream{fs: f, a: *al}
		if _, err := s.ReadAt(data, 0); err != nil && err != io.EOF {
			return nil, err
		}
	}
	le := binary.LittleEndian
	exts := map[uint64]bool{}
	for p := 0; p+26 <= len(data); {
		l := int(le.Uint16(data[p+4:]))
		if l < 26 {
			break
		}
		ref := le.Uint64(data[p+16:]) & refMask
		if ref != n {
			exts[ref] = true
		}
		p += l
	}
	var all []attr
	for _, a := range r.attrs {
		if a.typ != atAttrList {
			all = append(all, a)
		}
	}
	refs := make([]uint64, 0, len(exts))
	for e := range exts {
		refs = append(refs, e)
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i] < refs[j] })
	for _, e := range refs {
		er, err := f.readRaw(e)
		if err != nil {
			return nil, err
		}
		x, err := parseRecord(e, er)
		if err != nil {
			return nil, err
		}
		all = append(all, x.attrs...)
	}
	r.attrs = mergeAttrs(all)
	return r, nil
}

// mergeAttrs joins the pieces of non-resident attributes split across records.
func mergeAttrs(in []attr) []attr {
	type key struct {
		t uint32
		n string
	}
	var out []attr
	idx := map[key]int{}
	sort.SliceStable(in, func(i, j int) bool { return in[i].startVCN < in[j].startVCN })
	for _, a := range in {
		if !a.nonres {
			out = append(out, a)
			continue
		}
		k := key{a.typ, a.name}
		if i, ok := idx[k]; ok {
			out[i].runs = append(out[i].runs, a.runs...)
			continue
		}
		idx[k] = len(out)
		out = append(out, a)
	}
	return out
}

func filetime(v uint64) time.Time {
	if v == 0 {
		return time.Unix(0, 0)
	}
	const epochDiff = 116444736000000000
	t := int64(v) - epochDiff
	return time.Unix(t/10000000, (t%10000000)*100)
}

// reparseTag returns the reparse tag of a record (0 if none).
func reparseTag(r *record) uint32 {
	if a := r.find(atReparse, ""); a != nil && len(a.value) >= 8 {
		return binary.LittleEndian.Uint32(a.value)
	}
	return 0
}

// --- vfs.FS ---

func (f *FS) Type() string          { return "ntfs" }
func (f *FS) Label() string         { return f.label }
func (f *FS) Root() vfs.Node        { return uint64(rootRecord) }
func (f *FS) CaseInsensitive() bool { return true }

func (f *FS) Stat(n vfs.Node) (vfs.Stat, error) {
	r, err := f.rec(n.(uint64))
	if err != nil {
		return vfs.Stat{}, err
	}
	st := vfs.Stat{Mode: 0o644}
	if a := r.find(atStdInfo, ""); a != nil && len(a.value) >= 16 {
		st.Mtime = filetime(binary.LittleEndian.Uint64(a.value[8:]))
	}
	switch tag := reparseTag(r); tag {
	case tagSymlink, tagMountPoint:
		st.Mode = fs.ModeSymlink | 0o777
		return st, nil
	}
	switch k, _ := f.interix(r); k {
	case "link":
		st.Mode = fs.ModeSymlink | 0o777
		return st, nil
	case "fifo":
		st.Mode = fs.ModeNamedPipe | 0o644
		return st, nil
	case "socket":
		st.Mode = fs.ModeSocket | 0o644
		return st, nil
	case "device":
		st.Mode = fs.ModeDevice | 0o644
		return st, nil
	}
	if r.isDir() {
		st.Mode = fs.ModeDir | 0o755
		return st, nil
	}
	if a := r.find(atData, ""); a != nil {
		st.Size = a.size
	}
	return st, nil
}

func (f *FS) ReadDir(n vfs.Node) ([]vfs.Entry, error) {
	num := n.(uint64)
	f.mu.Lock()
	if e, ok := f.dirs.Get(num); ok {
		f.mu.Unlock()
		return e, nil
	}
	f.mu.Unlock()
	r, err := f.rec(num)
	if err != nil {
		return nil, err
	}
	if !r.isDir() {
		return nil, errors.New("ntfs: not a directory")
	}
	ir := r.find(atIndexRoot, "$I30")
	if ir == nil || len(ir.value) < 32 {
		return nil, errors.New("ntfs: directory has no $I30 index")
	}
	var alloc *stream
	if a := r.find(atIndexAlloc, "$I30"); a != nil {
		alloc = &stream{fs: f, a: *a}
	}
	blockSize := int64(binary.LittleEndian.Uint32(ir.value[8:]))
	if blockSize == 0 {
		blockSize = f.indexSize
	}
	vcnSize := f.clusterSize
	if blockSize < f.clusterSize {
		vcnSize = 512
	}
	w := &indexWalker{fs: f, alloc: alloc, blockSize: blockSize, vcnSize: vcnSize, seen: map[uint64]bool{}, refs: map[uint64]bool{}}
	if err := w.entries(ir.value[16:], 0); err != nil {
		return nil, fmt.Errorf("ntfs: directory %d: %w", num, err)
	}
	out := w.out
	if num == rootRecord {
		// hide the NTFS metadata files ($MFT, $Bitmap, $Extend, ...)
		k := 0
		for _, e := range out {
			if e.Node.(uint64) >= 16 || !strings.HasPrefix(e.Name, "$") {
				out[k] = e
				k++
			}
		}
		out = out[:k]
	}
	vfs.SortEntries(out)
	f.mu.Lock()
	f.dirs.Put(num, out)
	f.mu.Unlock()
	return out, nil
}

type indexWalker struct {
	fs        *FS
	alloc     *stream
	blockSize int64
	vcnSize   int64
	seen      map[uint64]bool // visited index blocks
	refs      map[uint64]bool // name+ref de-dup is done with names below
	out       []vfs.Entry
	names     map[string]bool
}

// entries parses an index node (header + entries), recursing into sub-nodes.
func (w *indexWalker) entries(hdr []byte, depth int) error {
	if depth > 32 || len(hdr) < 16 {
		return errors.New("bad index")
	}
	le := binary.LittleEndian
	p := int(le.Uint32(hdr))
	end := int(le.Uint32(hdr[4:]))
	if end > len(hdr) {
		end = len(hdr)
	}
	if w.names == nil {
		w.names = map[string]bool{}
	}
	for p+16 <= end {
		e := hdr[p:]
		elen := int(le.Uint16(e[8:]))
		klen := int(le.Uint16(e[10:]))
		flags := le.Uint16(e[12:])
		if elen < 16 || p+elen > end {
			break
		}
		if flags&1 != 0 && elen >= 24 { // sub-node first (keeps order irrelevant; we sort later)
			vcn := le.Uint64(e[elen-8:])
			if err := w.subnode(vcn, depth); err != nil {
				return err
			}
		}
		if flags&2 != 0 { // last entry: no key
			break
		}
		if klen >= 66 && 16+klen <= elen {
			w.add(le.Uint64(e)&refMask, e[16:16+klen])
		}
		p += elen
	}
	return nil
}

func (w *indexWalker) subnode(vcn uint64, depth int) error {
	if w.alloc == nil {
		return errors.New("index sub-node without $INDEX_ALLOCATION")
	}
	if w.seen[vcn] {
		return nil
	}
	w.seen[vcn] = true
	b := make([]byte, w.blockSize)
	if _, err := w.alloc.ReadAt(b, int64(vcn)*w.vcnSize); err != nil && err != io.EOF {
		return err
	}
	if string(b[:4]) != "INDX" {
		return fmt.Errorf("index block at vcn %d: bad signature", vcn)
	}
	if err := w.fs.fixup(b); err != nil {
		return err
	}
	return w.entries(b[0x18:], depth+1)
}

func (w *indexWalker) add(ref uint64, fn []byte) {
	nl := int(fn[0x40])
	ns := fn[0x41]
	if ns == 2 || 0x42+nl*2 > len(fn) { // DOS 8.3 alias
		return
	}
	name := utf16le(fn[0x42 : 0x42+nl*2])
	key := fmt.Sprintf("%d/%s", ref, name)
	if w.names[key] || name == "." {
		return
	}
	w.names[key] = true
	fl := binary.LittleEndian.Uint32(fn[0x38:])
	k := vfs.KindFile
	switch {
	case fl&0x400 != 0: // reparse point: symlink, junction or something else
		k = vfs.KindUnknown
	case fl&0x4 != 0 && fl&0x10000000 == 0: // system file: maybe an Interix symlink/fifo written by Linux
		k = vfs.KindUnknown
	case fl&0x10000000 != 0:
		k = vfs.KindDir
	}
	w.out = append(w.out, vfs.Entry{Name: name, Node: ref, Kind: k})
}

func (f *FS) Open(n vfs.Node) (vfs.File, error) {
	r, err := f.rec(n.(uint64))
	if err != nil {
		return nil, err
	}
	switch reparseTag(r) {
	case tagWOF:
		return nil, errors.New("ntfs: file is compressed with Windows' CompactOS (WOF), which is not supported")
	case tagDedup:
		return nil, errors.New("ntfs: file is stored by Data Deduplication, which is not supported")
	}
	a := r.find(atData, "")
	if a == nil {
		if r.isDir() {
			return nil, errors.New("ntfs: is a directory")
		}
		return &stream{fs: f, a: attr{}}, nil
	}
	if a.flags&flagEncrypted != 0 {
		return nil, errors.New("ntfs: file is encrypted (EFS)")
	}
	return &stream{fs: f, a: *a}, nil
}

// Parent returns the directory that holds n (from its $FILE_NAME attribute).
func (f *FS) Parent(n vfs.Node) (vfs.Node, error) {
	num := n.(uint64)
	if num == rootRecord {
		return num, nil
	}
	r, err := f.rec(num)
	if err != nil {
		return nil, err
	}
	for _, a := range r.attrs {
		if a.typ == atFileName && len(a.value) >= 0x42 && a.value[0x41] != 2 {
			return binary.LittleEndian.Uint64(a.value) & refMask, nil
		}
	}
	return uint64(rootRecord), nil
}

func (f *FS) Readlink(n vfs.Node) (string, error) {
	r, err := f.rec(n.(uint64))
	if err != nil {
		return "", err
	}
	a := r.find(atReparse, "")
	if a == nil {
		if k, t := f.interix(r); k == "link" {
			return t, nil
		}
	}
	if a == nil || len(a.value) < 16 {
		return "", errors.New("ntfs: not a symbolic link")
	}
	le := binary.LittleEndian
	v := a.value
	tag := le.Uint32(v)
	buf := 16
	relative := false
	if tag == tagSymlink {
		if len(v) < 20 {
			return "", errors.New("ntfs: bad symlink")
		}
		relative = le.Uint32(v[16:])&1 != 0
		buf = 20
	} else if tag != tagMountPoint {
		return "", errors.New("ntfs: unsupported reparse point")
	}
	so, sl := int(le.Uint16(v[8:])), int(le.Uint16(v[10:]))
	po, pl := int(le.Uint16(v[12:])), int(le.Uint16(v[14:]))
	off, ln := po, pl
	if pl == 0 {
		off, ln = so, sl
	}
	if buf+off+ln > len(v) {
		return "", errors.New("ntfs: bad reparse data")
	}
	t := utf16le(v[buf+off : buf+off+ln])
	if relative {
		return strings.ReplaceAll(t, `\`, "/"), nil
	}
	// absolute Windows path ("C:\Users\x" or "\??\C:\Users\x"): treat as relative to this volume's root
	t = strings.TrimPrefix(t, `\??\`)
	if len(t) >= 2 && t[1] == ':' {
		t = t[2:]
	}
	return "/" + strings.TrimLeft(strings.ReplaceAll(t, `\`, "/"), "/"), nil
}

// interix recognizes the special files ntfs-3g creates for Linux symlinks, FIFOs, sockets and devices:
// system files whose data starts with "IntxLNK\x01" (+ UTF-16 target), "IntxCHR\x00", "IntxBLK\x00",
// or that are empty (FIFO) or one byte long (socket).
func (f *FS) interix(r *record) (kind, target string) {
	si := r.find(atStdInfo, "")
	if si == nil || len(si.value) < 0x24 || r.isDir() || reparseTag(r) != 0 {
		return "", ""
	}
	if binary.LittleEndian.Uint32(si.value[0x20:])&0x4 == 0 { // FILE_ATTRIBUTE_SYSTEM
		return "", ""
	}
	d := r.find(atData, "")
	switch {
	case d == nil || d.size == 0:
		return "fifo", ""
	case d.size == 1:
		return "socket", ""
	case d.size > 8+2*4096:
		return "", ""
	}
	b := make([]byte, d.size)
	s := &stream{fs: f, a: *d}
	if _, err := s.ReadAt(b, 0); err != nil && err != io.EOF {
		return "", ""
	}
	switch string(b[:min(8, len(b))]) {
	case "IntxLNK\x01":
		return "link", utf16le(b[8:])
	case "IntxCHR\x00", "IntxBLK\x00":
		return "device", ""
	}
	return "", ""
}

func (f *FS) Info() []string {
	s := f.serial
	out := []string{
		fmt.Sprintf("serial: %04X-%04X", (s>>16)&0xFFFF, s&0xFFFF),
		fmt.Sprintf("size:  %.1f GiB   cluster size: %d   MFT record: %d", float64(f.totalBytes)/(1<<30), f.clusterSize, f.recordSize),
	}
	if f.version != "" {
		out = append(out, "NTFS version: "+f.version)
	}
	return out
}
