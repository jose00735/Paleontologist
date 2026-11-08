package ext

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"paleontologist/vfs"
	"sort"
)

// run maps n logical blocks starting at lblk to physical blocks starting at pblk.
type run struct {
	lblk, n, pblk uint64
	zero          bool // uninitialized (preallocated) extent
}

// File is an open ext file: a block map or inline data.
type File struct {
	fs     *FS
	size   int64
	runs   []run
	inline []byte
}

func (fl *File) Size() int64 { return fl.size }

func (f *FS) openInode(in *inode) (*File, error) {
	fl := &File{fs: f, size: in.size}
	if in.flags&flagEncrypt != 0 {
		return nil, errors.New("ext: encrypted file (fscrypt) can't be read")
	}
	if in.flags&flagCompr != 0 && f.incompat&incompatExtents == 0 {
		return nil, errors.New("ext: compressed ext2 files are not supported")
	}
	switch {
	case in.flags&flagInlineData != 0:
		d := append([]byte(nil), in.iblock...)
		d = append(d, f.inlineXattrData(in)...)
		if int64(len(d)) > in.size {
			d = d[:in.size]
		}
		fl.inline = d
		return fl, nil
	case in.flags&flagExtents != 0:
		if err := f.walkExtents(in.iblock, &fl.runs, 0); err != nil {
			return nil, fmt.Errorf("ext: inode %d: %w", in.num, err)
		}
	default:
		nblocks := uint64((in.size + f.bs - 1) / f.bs)
		m := &blockMapper{f: f, need: nblocks}
		le := binary.LittleEndian
		for i := 0; i < 12 && m.lblk < nblocks; i++ {
			m.add(uint64(le.Uint32(in.iblock[i*4:])))
		}
		per := uint64(f.bs / 4)
		span := per
		for lvl := 1; lvl <= 3 && m.lblk < nblocks; lvl++ {
			p := uint64(le.Uint32(in.iblock[(11+lvl)*4:]))
			if err := m.indirect(p, lvl, span); err != nil {
				return nil, fmt.Errorf("ext: inode %d: %w", in.num, err)
			}
			span *= per
		}
		fl.runs = m.runs
	}
	sort.Slice(fl.runs, func(i, j int) bool { return fl.runs[i].lblk < fl.runs[j].lblk })
	return fl, nil
}

func (f *FS) walkExtents(node []byte, out *[]run, depth int) error {
	le := binary.LittleEndian
	if len(node) < 12 || le.Uint16(node) != extentMagic {
		return errors.New("bad extent header")
	}
	if depth > 8 {
		return errors.New("extent tree too deep")
	}
	n := int(le.Uint16(node[2:]))
	lvl := le.Uint16(node[6:])
	for i := 0; i < n; i++ {
		e := node[12+i*12:]
		if len(e) < 12 {
			break
		}
		if lvl == 0 {
			l := uint64(le.Uint16(e[4:]))
			zero := false
			if l > 32768 {
				l -= 32768
				zero = true
			}
			p := uint64(le.Uint32(e[8:])) | uint64(le.Uint16(e[6:]))<<32
			*out = append(*out, run{lblk: uint64(le.Uint32(e)), n: l, pblk: p, zero: zero})
			continue
		}
		child := uint64(le.Uint32(e[4:])) | uint64(le.Uint16(e[8:]))<<32
		b := make([]byte, f.bs)
		if err := vfs.ReadFull(f.dev, b, int64(child)*f.bs); err != nil {
			return err
		}
		if err := f.walkExtents(b, out, depth+1); err != nil {
			return err
		}
	}
	return nil
}

// blockMapper flattens ext2/3 indirect block maps into runs.
type blockMapper struct {
	f    *FS
	lblk uint64
	need uint64
	runs []run
}

func (m *blockMapper) add(p uint64) {
	if p != 0 {
		if k := len(m.runs); k > 0 {
			r := &m.runs[k-1]
			if r.lblk+r.n == m.lblk && r.pblk+r.n == p {
				r.n++
				m.lblk++
				return
			}
		}
		m.runs = append(m.runs, run{lblk: m.lblk, n: 1, pblk: p})
	}
	m.lblk++
}

// indirect walks an indirect block of the given level; span is the number of data blocks it covers.
func (m *blockMapper) indirect(p uint64, lvl int, span uint64) error {
	if p == 0 {
		m.lblk += span
		return nil
	}
	b := make([]byte, m.f.bs)
	if err := vfs.ReadFull(m.f.dev, b, int64(p)*m.f.bs); err != nil {
		return err
	}
	per := uint64(m.f.bs / 4)
	child := span / per
	for i := uint64(0); i < per && m.lblk < m.need; i++ {
		q := uint64(binary.LittleEndian.Uint32(b[i*4:]))
		if lvl == 1 {
			m.add(q)
		} else if err := m.indirect(q, lvl-1, child); err != nil {
			return err
		}
	}
	return nil
}

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
	if fl.inline != nil || len(fl.runs) == 0 && fl.size <= 60 {
		n := 0
		if off < int64(len(fl.inline)) {
			n = copy(p[:want], fl.inline[off:])
		}
		clear(p[n:want])
		return want, eof
	}
	bs := uint64(fl.fs.bs)
	done := 0
	for done < want {
		cur := uint64(off) + uint64(done)
		lb := cur / bs
		i := sort.Search(len(fl.runs), func(i int) bool { return fl.runs[i].lblk > lb }) - 1
		if i < 0 || lb >= fl.runs[i].lblk+fl.runs[i].n {
			// hole until the next run
			end := uint64(off) + uint64(want)
			if i+1 < len(fl.runs) && fl.runs[i+1].lblk*bs < end {
				end = fl.runs[i+1].lblk * bs
			}
			n := int(end - cur)
			clear(p[done : done+n])
			done += n
			continue
		}
		r := fl.runs[i]
		runEnd := (r.lblk + r.n) * bs
		n := want - done
		if uint64(n) > runEnd-cur {
			n = int(runEnd - cur)
		}
		if r.zero {
			clear(p[done : done+n])
		} else {
			phys := int64(r.pblk*bs + (cur - r.lblk*bs))
			if err := vfs.ReadFull(fl.fs.dev, p[done:done+n], phys); err != nil {
				return done, err
			}
		}
		done += n
	}
	return done, eof
}
