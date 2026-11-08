package btrfs

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

// FS is an opened btrfs filesystem.
type FS struct {
	dev   io.ReaderAt
	sb    *Super
	m     mapper
	nodes nodeCache

	mu    sync.Mutex
	roots map[uint64]uint64 // tree id -> root node logical address
	dirs  map[Ino][]DirEntry
	dirQ  []Ino
}

// Ino identifies an inode: the subvolume tree it lives in and its inode number.
type Ino struct {
	Tree uint64
	Num  uint64
}

// RootIno is the root directory of the top-level subvolume.
var RootIno = Ino{FSTreeID, FirstFreeID}

// Open reads the superblock and chunk tree of a btrfs filesystem.
func Open(dev io.ReaderAt) (*FS, error) {
	b := make([]byte, 4096)
	if _, err := dev.ReadAt(b, superOffset); err != nil {
		return nil, fmt.Errorf("btrfs: reading superblock: %w", err)
	}
	sb, err := parseSuper(b)
	if err != nil {
		return nil, err
	}
	f := &FS{dev: dev, sb: sb, roots: map[uint64]uint64{}, dirs: map[Ino][]DirEntry{}}
	f.nodes = nodeCache{max: 2048, m: map[uint64]*node{}}
	f.m.devID = sb.DevID
	// bootstrap chunks from the superblock
	arr := sb.sysChunks
	for len(arr) > 0 {
		if len(arr) < 17 {
			return nil, errors.New("btrfs: truncated sys_chunk_array")
		}
		k := readKey(arr)
		c, n, err := parseChunk(k.Offset, arr[17:])
		if err != nil {
			return nil, err
		}
		f.m.add(c)
		arr = arr[17+n:]
	}
	// full chunk tree
	err = f.walk(sb.ChunkRoot, Key{firstChunkTreeID, typeChunkItem, 0}, Key{firstChunkTreeID, typeChunkItem, math.MaxUint64},
		func(k Key, d []byte) error {
			c, _, err := parseChunk(k.Offset, d)
			if err != nil {
				return err
			}
			f.m.add(c)
			return nil
		})
	if err != nil {
		return nil, fmt.Errorf("btrfs: chunk tree: %w", err)
	}
	f.roots[RootTreeID] = sb.Root
	return f, nil
}

// Super returns the superblock.
func (f *FS) Super() *Super { return f.sb }

// treeRoot returns the root node address of a subvolume tree.
func (f *FS) treeRoot(id uint64) (uint64, error) {
	f.mu.Lock()
	if r, ok := f.roots[id]; ok {
		f.mu.Unlock()
		return r, nil
	}
	f.mu.Unlock()
	var found []byte
	err := f.items(f.sb.Root, id, typeRootItem, func(_ Key, d []byte) error {
		found = d // keep the last (newest) one
		return nil
	})
	if err != nil {
		return 0, err
	}
	if len(found) < 239 {
		return 0, fmt.Errorf("btrfs: subvolume %d not found", id)
	}
	r := binary.LittleEndian.Uint64(found[176:])
	f.mu.Lock()
	f.roots[id] = r
	f.mu.Unlock()
	return r, nil
}

// Stat describes an inode.
type Stat struct {
	Ino   Ino
	Size  int64
	Mode  uint32 // Unix mode bits including type
	UID   uint32
	GID   uint32
	Nlink uint32
	Mtime time.Time
	Ctime time.Time
	Atime time.Time
}

// IsDir reports whether the inode is a directory.
func (s Stat) IsDir() bool { return s.Mode&0o170000 == 0o040000 }

// IsSymlink reports whether the inode is a symbolic link.
func (s Stat) IsSymlink() bool { return s.Mode&0o170000 == 0o120000 }

// IsRegular reports whether the inode is a regular file.
func (s Stat) IsRegular() bool { return s.Mode&0o170000 == 0o100000 }

// FileMode converts to io/fs mode bits.
func (s Stat) FileMode() fs.FileMode {
	m := fs.FileMode(s.Mode & 0o777)
	switch s.Mode & 0o170000 {
	case 0o040000:
		m |= fs.ModeDir
	case 0o120000:
		m |= fs.ModeSymlink
	case 0o010000:
		m |= fs.ModeNamedPipe
	case 0o140000:
		m |= fs.ModeSocket
	case 0o020000:
		m |= fs.ModeDevice | fs.ModeCharDevice
	case 0o060000:
		m |= fs.ModeDevice
	}
	return m
}

func ts(b []byte) time.Time {
	sec := int64(binary.LittleEndian.Uint64(b))
	nsec := int64(binary.LittleEndian.Uint32(b[8:]))
	return time.Unix(sec, nsec)
}

// Stat reads an inode item.
func (f *FS) Stat(ino Ino) (Stat, error) {
	root, err := f.treeRoot(ino.Tree)
	if err != nil {
		return Stat{}, err
	}
	d, err := f.lookup(root, Key{ino.Num, typeInodeItem, 0})
	if err != nil {
		return Stat{}, err
	}
	if len(d) < 160 {
		return Stat{}, fs.ErrNotExist
	}
	le := binary.LittleEndian
	return Stat{
		Ino:   ino,
		Size:  int64(le.Uint64(d[16:])),
		Nlink: le.Uint32(d[40:]),
		UID:   le.Uint32(d[44:]),
		GID:   le.Uint32(d[48:]),
		Mode:  le.Uint32(d[52:]),
		Atime: ts(d[112:]),
		Ctime: ts(d[124:]),
		Mtime: ts(d[136:]),
	}, nil
}

// DirEntry is one directory entry.
type DirEntry struct {
	Name   string
	Ino    Ino
	Type   uint8 // BTRFS_FT_*: 1 file, 2 dir, 3 chr, 4 blk, 5 fifo, 6 sock, 7 symlink
	Subvol bool  // entry is a subvolume or snapshot root
}

// ReadDir lists a directory (cached).
func (f *FS) ReadDir(dir Ino) ([]DirEntry, error) {
	f.mu.Lock()
	if e, ok := f.dirs[dir]; ok {
		f.mu.Unlock()
		return e, nil
	}
	f.mu.Unlock()
	root, err := f.treeRoot(dir.Tree)
	if err != nil {
		return nil, err
	}
	var out []DirEntry
	err = f.items(root, dir.Num, typeDirIndex, func(_ Key, d []byte) error {
		if len(d) < 30 {
			return nil
		}
		loc := readKey(d)
		le := binary.LittleEndian
		dataLen := int(le.Uint16(d[25:]))
		nameLen := int(le.Uint16(d[27:]))
		typ := d[29]
		if 30+nameLen+dataLen > len(d) {
			return nil
		}
		e := DirEntry{Name: string(d[30 : 30+nameLen]), Type: typ}
		if loc.Type == typeRootItem {
			if _, err := f.treeRoot(loc.ObjectID); err != nil {
				return nil // dangling entry of a deleted subvolume
			}
			e.Ino = Ino{loc.ObjectID, FirstFreeID}
			e.Subvol = true
			e.Type = 2
		} else {
			e.Ino = Ino{dir.Tree, loc.ObjectID}
		}
		out = append(out, e)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	f.mu.Lock()
	if len(f.dirQ) >= 4096 {
		delete(f.dirs, f.dirQ[0])
		f.dirQ = f.dirQ[1:]
	}
	f.dirs[dir] = out
	f.dirQ = append(f.dirQ, dir)
	f.mu.Unlock()
	return out, nil
}

// Lookup finds name inside dir.
func (f *FS) Lookup(dir Ino, name string) (DirEntry, error) {
	ents, err := f.ReadDir(dir)
	if err != nil {
		return DirEntry{}, err
	}
	i := sort.Search(len(ents), func(i int) bool { return ents[i].Name >= name })
	if i < len(ents) && ents[i].Name == name {
		return ents[i], nil
	}
	return DirEntry{}, fs.ErrNotExist
}

// Resolve walks a slash-separated path from start. Symlinks are followed when follow is true
// (always for intermediate components); absolute symlink targets are resolved from the root of
// the subvolume that contains the link, which matches how the subvolume was mounted as "/".
func (f *FS) Resolve(start Ino, path string, follow bool) (Ino, error) {
	return f.resolve(start, path, follow, 0)
}

func (f *FS) resolve(start Ino, path string, follow bool, hops int) (Ino, error) {
	if hops > 32 {
		return Ino{}, errors.New("btrfs: too many levels of symbolic links")
	}
	cur := start
	parts := strings.Split(path, "/")
	for i, p := range parts {
		if p == "" || p == "." {
			continue
		}
		if p == ".." {
			par, err := f.Parent(cur)
			if err != nil {
				return Ino{}, err
			}
			cur = par
			continue
		}
		e, err := f.Lookup(cur, p)
		if err != nil {
			return Ino{}, err
		}
		last := i == len(parts)-1 || allEmpty(parts[i+1:])
		if e.Type == 7 && (!last || follow) {
			target, err := f.Readlink(e.Ino)
			if err != nil {
				return Ino{}, err
			}
			base := cur
			if strings.HasPrefix(target, "/") {
				base = Ino{cur.Tree, FirstFreeID}
			}
			t, err := f.resolve(base, target, true, hops+1)
			if err != nil {
				return Ino{}, err
			}
			cur = t
			continue
		}
		cur = e.Ino
	}
	return cur, nil
}

// Parent returns the directory that contains ino (the root is its own parent).
func (f *FS) Parent(ino Ino) (Ino, error) {
	if ino.Num == FirstFreeID {
		if ino.Tree == FSTreeID {
			return ino, nil
		}
		// subvolume root: ROOT_BACKREF (tree, 144, parentTree) -> dirid
		var par Ino
		err := f.items(f.sb.Root, ino.Tree, 144, func(k Key, d []byte) error {
			if len(d) >= 8 {
				par = Ino{k.Offset, binary.LittleEndian.Uint64(d)}
			}
			return errStop
		})
		if err != nil {
			return Ino{}, err
		}
		if par.Num == 0 {
			return RootIno, nil
		}
		return par, nil
	}
	root, err := f.treeRoot(ino.Tree)
	if err != nil {
		return Ino{}, err
	}
	par := Ino{}
	err = f.items(root, ino.Num, typeInodeRef, func(k Key, _ []byte) error {
		par = Ino{ino.Tree, k.Offset}
		return errStop
	})
	if err != nil {
		return Ino{}, err
	}
	if par.Num == 0 {
		return Ino{ino.Tree, FirstFreeID}, nil
	}
	return par, nil
}

// Path returns the full path of an inode from the top level (without a leading slash).
func (f *FS) Path(ino Ino) (string, error) {
	var parts []string
	cur := ino
	for g := 0; g < 4096; g++ {
		if cur == RootIno {
			break
		}
		name, par, err := f.nameAndParent(cur)
		if err != nil {
			return "", err
		}
		parts = append(parts, name)
		cur = par
	}
	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	return strings.Join(parts, "/"), nil
}

func (f *FS) nameAndParent(ino Ino) (string, Ino, error) {
	le := binary.LittleEndian
	var name string
	var par Ino
	if ino.Num == FirstFreeID && ino.Tree != FSTreeID {
		err := f.items(f.sb.Root, ino.Tree, 144, func(k Key, d []byte) error {
			if len(d) >= 18 {
				nl := int(le.Uint16(d[16:]))
				if 18+nl <= len(d) {
					name = string(d[18 : 18+nl])
					par = Ino{k.Offset, le.Uint64(d)}
				}
			}
			return errStop
		})
		if err != nil || name == "" {
			return "", Ino{}, fs.ErrNotExist
		}
		return name, par, nil
	}
	root, err := f.treeRoot(ino.Tree)
	if err != nil {
		return "", Ino{}, err
	}
	err = f.items(root, ino.Num, typeInodeRef, func(k Key, d []byte) error {
		if len(d) >= 10 {
			nl := int(le.Uint16(d[8:]))
			if 10+nl <= len(d) {
				name = string(d[10 : 10+nl])
				par = Ino{ino.Tree, k.Offset}
			}
		}
		return errStop
	})
	if err != nil || name == "" {
		return "", Ino{}, fs.ErrNotExist
	}
	return name, par, nil
}

func allEmpty(p []string) bool {
	for _, s := range p {
		if s != "" && s != "." {
			return false
		}
	}
	return true
}

// Readlink returns a symlink's target.
func (f *FS) Readlink(ino Ino) (string, error) {
	st, err := f.Stat(ino)
	if err != nil {
		return "", err
	}
	if st.Size > 4096 {
		return "", errors.New("btrfs: symlink target too long")
	}
	r, err := f.OpenFile(ino)
	if err != nil {
		return "", err
	}
	b := make([]byte, st.Size)
	n, err := r.ReadAt(b, 0)
	if err != nil && err != io.EOF {
		return "", err
	}
	return string(b[:n]), nil
}

// Subvolume describes a subvolume or snapshot.
type Subvolume struct {
	ID     uint64
	Parent uint64
	Name   string
	Path   string
}

// Subvolumes lists every subvolume reachable from the top level.
func (f *FS) Subvolumes() ([]Subvolume, error) {
	var out []Subvolume
	err := f.walk(f.sb.Root, Key{0, 0, 0}, Key{math.MaxUint64, 255, math.MaxUint64}, func(k Key, d []byte) error {
		if k.Type != typeRootRef || len(d) < 18 {
			return nil
		}
		nl := int(binary.LittleEndian.Uint16(d[16:]))
		if 18+nl > len(d) {
			return nil
		}
		out = append(out, Subvolume{ID: k.Offset, Parent: k.ObjectID, Name: string(d[18 : 18+nl])})
		return nil
	})
	if err != nil {
		return nil, err
	}
	for i := range out {
		if p, err := f.Path(Ino{out[i].ID, FirstFreeID}); err == nil {
			out[i].Path = p
		} else {
			out[i].Path = out[i].Name
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}
