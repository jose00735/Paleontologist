// Package vmdk opens VMware virtual disks (and raw images) as a read-only io.ReaderAt.
//
// Supported: monolithicSparse, twoGbMaxExtentSparse, streamOptimized (compressed grains),
// monolithicFlat, twoGbMaxExtentFlat, vmfs / vmfsThin (flat -flat.vmdk), ZERO extents,
// snapshot chains of hosted sparse disks (parentFileNameHint) and plain raw images.
// Not supported: ESXi delta formats (vmfsSparse "COWD", seSparse) and encrypted disks.
package vmdk

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

const sector = 512

// Disk is a read-only virtual disk.
type Disk interface {
	io.ReaderAt
	Size() int64
	Close() error
	// Describe returns a human readable description (format, files, parents).
	Describe() string
}

// Open opens a .vmdk descriptor, a sparse .vmdk, a flat extent or a raw image.
func Open(path string) (Disk, error) {
	return open(path, 0)
}

func open(path string, depth int) (Disk, error) {
	if depth > 32 {
		return nil, errors.New("vmdk: snapshot chain too deep (loop?)")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	head := make([]byte, 4096)
	n, _ := f.ReadAt(head, 0)
	head = head[:n]

	switch {
	case n >= 4 && string(head[:4]) == "KDMV":
		// Hosted sparse extent, possibly with an embedded descriptor.
		sp, err := openSparse(f, filepath.Base(path))
		if err != nil {
			f.Close()
			return nil, err
		}
		d := &composite{name: path, kind: "sparse", size: int64(sp.h.Capacity) * sector}
		var desc *descriptor
		if sp.h.DescriptorOffset != 0 && sp.h.DescriptorSize != 0 && sp.h.DescriptorSize < 4096 {
			buf := make([]byte, sp.h.DescriptorSize*sector)
			if _, err := f.ReadAt(buf, int64(sp.h.DescriptorOffset)*sector); err == nil || err == io.EOF {
				desc, _ = parseDescriptor(buf)
			}
		}
		if desc != nil && desc.createType != "" {
			d.kind = desc.createType
		}
		d.extents = []extent{{start: 0, size: d.size, r: sp, file: f, desc: "SPARSE " + filepath.Base(path)}}
		if desc != nil && desc.parentHint != "" {
			p, err := openParent(path, desc.parentHint, depth)
			if err != nil {
				d.Close()
				return nil, err
			}
			d.parent = p
			sp.parent = p
		}
		return d, nil
	case n >= 4 && string(head[:4]) == "COWD":
		f.Close()
		return nil, errors.New("vmdk: ESXi vmfsSparse (COWD) delta disks are not supported; consolidate the snapshots first")
	case looksLikeDescriptor(head):
		f.Close()
		return openDescriptor(path, depth)
	default:
		// Raw image or a flat extent opened directly.
		return &composite{name: path, kind: "raw image", size: st.Size(),
			extents: []extent{{start: 0, size: st.Size(), r: f, file: f, desc: "RAW " + filepath.Base(path)}}}, nil
	}
}

func openParent(child, hint string, depth int) (Disk, error) {
	dir := filepath.Dir(child)
	hint = strings.ReplaceAll(hint, "\\", string(filepath.Separator))
	cands := []string{}
	if filepath.IsAbs(hint) {
		cands = append(cands, hint)
	} else {
		cands = append(cands, filepath.Join(dir, hint))
	}
	// Parent paths often point to the original machine; try the same folder.
	base := hint
	if i := strings.LastIndexAny(base, "/\\"); i >= 0 {
		base = base[i+1:]
	}
	cands = append(cands, filepath.Join(dir, base))
	for _, c := range cands {
		if _, err := os.Stat(c); err == nil {
			return open(c, depth+1)
		}
	}
	return nil, fmt.Errorf("vmdk: parent disk %q not found (needed by snapshot %s); put it in the same folder", hint, filepath.Base(child))
}

func looksLikeDescriptor(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	s := string(b)
	if !strings.Contains(s, "createType") && !strings.Contains(s, "# Disk DescriptorFile") {
		return false
	}
	for _, c := range b {
		if c == 0 {
			break
		}
		if c < 9 || (c > 13 && c < 32 && c != 27) {
			return false
		}
	}
	return true
}

type descriptor struct {
	createType string
	parentHint string
	extents    []extentLine
}

type extentLine struct {
	access  string
	sectors int64
	typ     string
	file    string
	offset  int64
}

var extentRe = regexp.MustCompile(`^(RW|RDONLY|NOACCESS)\s+(\d+)\s+([A-Za-z]+)(?:\s+"([^"]*)"(?:\s+(\d+))?)?`)

func parseDescriptor(b []byte) (*descriptor, error) {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	d := &descriptor{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if m := extentRe.FindStringSubmatch(line); m != nil {
			n, _ := strconv.ParseInt(m[2], 10, 64)
			off := int64(0)
			if m[5] != "" {
				off, _ = strconv.ParseInt(m[5], 10, 64)
			}
			d.extents = append(d.extents, extentLine{access: m[1], sectors: n, typ: strings.ToUpper(m[3]), file: m[4], offset: off})
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			k = strings.TrimSpace(k)
			v = strings.Trim(strings.TrimSpace(v), `"`)
			switch k {
			case "createType":
				d.createType = v
			case "parentFileNameHint":
				d.parentHint = v
			}
		}
	}
	return d, sc.Err()
}

func openDescriptor(path string, depth int) (Disk, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	desc, err := parseDescriptor(b)
	if err != nil {
		return nil, err
	}
	if len(desc.extents) == 0 {
		return nil, errors.New("vmdk: descriptor has no extents")
	}
	d := &composite{name: path, kind: desc.createType}
	dir := filepath.Dir(path)
	var pos int64
	var sparses []*sparseExtent
	for _, e := range desc.extents {
		size := e.sectors * sector
		ex := extent{start: pos, size: size, desc: e.typ + " " + e.file}
		switch e.typ {
		case "ZERO":
			ex.r = zeroReader{}
		case "FLAT", "VMFS", "VMFSRAW", "VMFSRDM":
			fp := filepath.Join(dir, e.file)
			f, err := os.Open(fp)
			if err != nil {
				d.Close()
				return nil, fmt.Errorf("vmdk: extent %s: %w", e.file, err)
			}
			ex.file = f
			ex.r = &offsetReader{r: f, off: e.offset * sector}
		case "SPARSE":
			fp := filepath.Join(dir, e.file)
			f, err := os.Open(fp)
			if err != nil {
				d.Close()
				return nil, fmt.Errorf("vmdk: extent %s: %w", e.file, err)
			}
			sp, err := openSparse(f, e.file)
			if err != nil {
				f.Close()
				d.Close()
				return nil, err
			}
			sp.base = pos
			ex.file = f
			ex.r = sp
			sparses = append(sparses, sp)
		case "VMFSSPARSE", "SESPARSE":
			d.Close()
			return nil, fmt.Errorf("vmdk: ESXi %s snapshot extents are not supported; consolidate the snapshots first", e.typ)
		default:
			d.Close()
			return nil, fmt.Errorf("vmdk: unsupported extent type %s", e.typ)
		}
		d.extents = append(d.extents, ex)
		pos += size
	}
	d.size = pos
	if desc.parentHint != "" {
		p, err := openParent(path, desc.parentHint, depth)
		if err != nil {
			d.Close()
			return nil, err
		}
		d.parent = p
		for _, sp := range sparses {
			sp.parent = p
		}
	}
	return d, nil
}

type extent struct {
	start, size int64
	r           io.ReaderAt // reads relative to the extent start
	file        *os.File
	desc        string
}

type composite struct {
	name    string
	kind    string
	size    int64
	extents []extent
	parent  Disk
}

func (d *composite) Size() int64 { return d.size }

func (d *composite) Describe() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s (%s, %d MiB)", filepath.Base(d.name), d.kind, d.size>>20)
	if len(d.extents) > 1 {
		fmt.Fprintf(&b, ", %d extents", len(d.extents))
	}
	if d.parent != nil {
		fmt.Fprintf(&b, "\n  snapshot of: %s", d.parent.Describe())
	}
	return b.String()
}

func (d *composite) Close() error {
	for _, e := range d.extents {
		if e.file != nil {
			e.file.Close()
		}
	}
	if d.parent != nil {
		d.parent.Close()
	}
	return nil
}

func (d *composite) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("vmdk: negative offset")
	}
	if off >= d.size {
		return 0, io.EOF
	}
	want := len(p)
	var eof error
	if off+int64(want) > d.size {
		want = int(d.size - off)
		eof = io.EOF
	}
	done := 0
	for done < want {
		cur := off + int64(done)
		e := d.find(cur)
		if e == nil {
			return done, fmt.Errorf("vmdk: offset %d outside extents", cur)
		}
		rel := cur - e.start
		n := want - done
		if int64(n) > e.size-rel {
			n = int(e.size - rel)
		}
		m, err := e.r.ReadAt(p[done:done+n], rel)
		if m < n {
			// flat extents may be shorter than declared (thin files): pad with zeros
			if err == io.EOF || err == nil {
				clear(p[done+m : done+n])
			} else {
				return done + m, err
			}
		}
		done += n
	}
	return done, eof
}

func (d *composite) find(off int64) *extent {
	lo, hi := 0, len(d.extents)-1
	for lo <= hi {
		mid := (lo + hi) / 2
		e := &d.extents[mid]
		switch {
		case off < e.start:
			hi = mid - 1
		case off >= e.start+e.size:
			lo = mid + 1
		default:
			return e
		}
	}
	return nil
}

type zeroReader struct{}

func (zeroReader) ReadAt(p []byte, off int64) (int, error) {
	clear(p)
	return len(p), nil
}

type offsetReader struct {
	r   io.ReaderAt
	off int64
}

func (o *offsetReader) ReadAt(p []byte, off int64) (int, error) {
	return o.r.ReadAt(p, o.off+off)
}
