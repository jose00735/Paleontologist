// Package vfs is the small read-only filesystem interface shared by every filesystem reader
// (btrfs, ext2/3/4, NTFS, FAT) and used by the WebDAV server, ls and extract.
package vfs

import (
	"errors"
	"io"
	"io/fs"
	"sort"
	"strings"
	"time"
)

// Node identifies a file or directory inside one filesystem. It must be comparable.
type Node any

// Kind is the type of a directory entry.
type Kind uint8

const (
	KindUnknown Kind = iota // call Stat to find out
	KindFile
	KindDir
	KindSymlink
	KindOther // devices, fifos, sockets
)

// Entry is one directory entry.
type Entry struct {
	Name string
	Node Node
	Kind Kind
	Note string // e.g. "subvolume"
}

// Stat describes a node.
type Stat struct {
	Size  int64
	Mode  fs.FileMode // permission bits plus type bits (ModeDir, ModeSymlink, ...)
	Mtime time.Time
}

func (s Stat) IsDir() bool     { return s.Mode.IsDir() }
func (s Stat) IsRegular() bool { return s.Mode.IsRegular() }
func (s Stat) IsSymlink() bool { return s.Mode&fs.ModeSymlink != 0 }

// File is an open file.
type File interface {
	io.ReaderAt
	Size() int64
}

// FS is a read-only filesystem.
type FS interface {
	Type() string  // "btrfs", "ext4", "ntfs", "fat32", ...
	Label() string // volume label, may be empty
	Root() Node
	Stat(Node) (Stat, error)
	ReadDir(Node) ([]Entry, error) // sorted by Name, without "." and ".."
	Open(Node) (File, error)
	Readlink(Node) (string, error) // slash-separated target
}

// Parenter is implemented by filesystems that can find a directory's parent.
type Parenter interface {
	Parent(Node) (Node, error)
}

// SymlinkRooter tells where absolute symlinks start (e.g. a btrfs subvolume root).
type SymlinkRooter interface {
	SymlinkRoot(Node) Node
}

// CaseInsensitive is implemented by filesystems whose names are case-insensitive (FAT, NTFS).
type CaseInsensitive interface {
	CaseInsensitive() bool
}

// Infoer adds filesystem-specific lines to "paleontologist info".
type Infoer interface {
	Info() []string
}

var ErrNotExist = fs.ErrNotExist

// SortEntries sorts entries by name.
func SortEntries(e []Entry) {
	sort.Slice(e, func(i, j int) bool { return e[i].Name < e[j].Name })
}

// Lookup finds name in dir.
func Lookup(f FS, dir Node, name string) (Entry, error) {
	ents, err := f.ReadDir(dir)
	if err != nil {
		return Entry{}, err
	}
	i := sort.Search(len(ents), func(i int) bool { return ents[i].Name >= name })
	if i < len(ents) && ents[i].Name == name {
		return ents[i], nil
	}
	if ci, ok := f.(CaseInsensitive); ok && ci.CaseInsensitive() {
		for _, e := range ents {
			if strings.EqualFold(e.Name, name) {
				return e, nil
			}
		}
	}
	return Entry{}, ErrNotExist
}

// Resolve walks a slash-separated path from start. Symlinks in the middle are always followed;
// the last component is followed only when follow is true.
func Resolve(f FS, start Node, path string, follow bool) (Node, error) {
	n, _, err := resolve(f, start, nil, path, follow, 0)
	return n, err
}

// stack holds the directories above start (when known), so ".." works without a parent lookup.
// It returns the node and the directories above it.
func resolve(f FS, start Node, stack []Node, path string, follow bool, hops int) (Node, []Node, error) {
	if hops > 32 {
		return nil, nil, errors.New("too many levels of symbolic links")
	}
	cur := start
	stack = append([]Node(nil), stack...)
	parts := strings.Split(path, "/")
	for i, p := range parts {
		if p == "" || p == "." {
			continue
		}
		if p == ".." {
			if len(stack) > 0 {
				cur = stack[len(stack)-1]
				stack = stack[:len(stack)-1]
			} else if pa, ok := f.(Parenter); ok {
				n, err := pa.Parent(cur)
				if err != nil {
					return nil, nil, err
				}
				cur = n
			}
			continue
		}
		e, err := Lookup(f, cur, p)
		if err != nil {
			return nil, nil, err
		}
		last := true
		for _, q := range parts[i+1:] {
			if q != "" && q != "." {
				last = false
				break
			}
		}
		kind := e.Kind
		if kind == KindUnknown {
			st, err := f.Stat(e.Node)
			if err != nil {
				return nil, nil, err
			}
			if st.IsSymlink() {
				kind = KindSymlink
			}
		}
		if kind == KindSymlink && (!last || follow) {
			target, err := f.Readlink(e.Node)
			if err != nil {
				return nil, nil, err
			}
			base, bstack := cur, stack
			if strings.HasPrefix(target, "/") {
				base, bstack = f.Root(), nil
				if sr, ok := f.(SymlinkRooter); ok {
					base = sr.SymlinkRoot(cur)
				}
			}
			t, ts, err := resolve(f, base, bstack, target, true, hops+1)
			if err != nil {
				return nil, nil, err
			}
			cur, stack = t, ts
			continue
		}
		stack = append(stack, cur)
		cur = e.Node
	}
	return cur, stack, nil
}

// Cache is a small FIFO cache for directory listings and similar data.
type Cache[K comparable, V any] struct {
	max   int
	m     map[K]V
	order []K
}

func NewCache[K comparable, V any](max int) *Cache[K, V] {
	return &Cache[K, V]{max: max, m: map[K]V{}}
}

func (c *Cache[K, V]) Get(k K) (V, bool) {
	v, ok := c.m[k]
	return v, ok
}

func (c *Cache[K, V]) Put(k K, v V) {
	if _, ok := c.m[k]; ok {
		c.m[k] = v
		return
	}
	if len(c.order) >= c.max {
		delete(c.m, c.order[0])
		c.order = c.order[1:]
	}
	c.order = append(c.order, k)
	c.m[k] = v
}

// ErrTruncated is returned when filesystem data points past the end of the disk image.
var ErrTruncated = errors.New("data lies past the end of the disk image (is the .vmdk incomplete or truncated?)")

// ReadFull reads exactly len(p) bytes at off from a device; short reads become ErrTruncated.
func ReadFull(r io.ReaderAt, p []byte, off int64) error {
	n, err := r.ReadAt(p, off)
	if n == len(p) {
		return nil
	}
	if err == nil || err == io.EOF {
		return ErrTruncated
	}
	return err
}
