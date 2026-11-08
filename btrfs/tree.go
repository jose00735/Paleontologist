package btrfs

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sync"
)

// Key is a btrfs item key.
type Key struct {
	ObjectID uint64
	Type     uint8
	Offset   uint64
}

func (a Key) cmp(b Key) int {
	switch {
	case a.ObjectID < b.ObjectID:
		return -1
	case a.ObjectID > b.ObjectID:
		return 1
	case a.Type < b.Type:
		return -1
	case a.Type > b.Type:
		return 1
	case a.Offset < b.Offset:
		return -1
	case a.Offset > b.Offset:
		return 1
	}
	return 0
}

func readKey(b []byte) Key {
	le := binary.LittleEndian
	return Key{ObjectID: le.Uint64(b[0:]), Type: b[8], Offset: le.Uint64(b[9:])}
}

const headerSize = 101

type node struct {
	level uint8
	keys  []Key
	ptrs  []uint64 // internal nodes
	data  [][]byte // leaves: item payloads
}

var errStop = errors.New("stop")

// nodeCache is a small LRU-ish (clock) cache of parsed tree nodes.
type nodeCache struct {
	mu    sync.Mutex
	max   int
	m     map[uint64]*node
	order []uint64
}

func (c *nodeCache) get(k uint64) *node {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.m[k]
}

func (c *nodeCache) put(k uint64, n *node) {
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
	c.m[k] = n
}

func (fs *FS) readNode(logical uint64) (*node, error) {
	if n := fs.nodes.get(logical); n != nil {
		return n, nil
	}
	b := make([]byte, fs.sb.NodeSize)
	if err := fs.readLogical(b, logical); err != nil {
		return nil, err
	}
	n, err := parseNode(b, logical)
	if err != nil {
		return nil, err
	}
	fs.nodes.put(logical, n)
	return n, nil
}

func parseNode(b []byte, logical uint64) (*node, error) {
	le := binary.LittleEndian
	if le.Uint64(b[0x30:]) != logical {
		return nil, fmt.Errorf("btrfs: tree block at %#x has wrong address (corrupt or wrong device)", logical)
	}
	n := &node{level: b[0x64]}
	nr := int(le.Uint32(b[0x60:]))
	if n.level == 0 {
		if headerSize+nr*25 > len(b) {
			return nil, fmt.Errorf("btrfs: bad leaf at %#x", logical)
		}
		n.keys = make([]Key, nr)
		n.data = make([][]byte, nr)
		for i := 0; i < nr; i++ {
			it := b[headerSize+i*25:]
			n.keys[i] = readKey(it)
			off := int(le.Uint32(it[17:]))
			size := int(le.Uint32(it[21:]))
			start := headerSize + off
			if start < headerSize || start+size > len(b) {
				return nil, fmt.Errorf("btrfs: bad item in leaf %#x", logical)
			}
			n.data[i] = b[start : start+size]
		}
		return n, nil
	}
	if headerSize+nr*33 > len(b) {
		return nil, fmt.Errorf("btrfs: bad node at %#x", logical)
	}
	n.keys = make([]Key, nr)
	n.ptrs = make([]uint64, nr)
	for i := 0; i < nr; i++ {
		kp := b[headerSize+i*33:]
		n.keys[i] = readKey(kp)
		n.ptrs[i] = le.Uint64(kp[17:])
	}
	return n, nil
}

// walk calls fn for every item with min <= key <= max in the tree rooted at root,
// in key order. fn may return errStop to end the walk early.
func (fs *FS) walk(root uint64, min, max Key, fn func(k Key, data []byte) error) error {
	err := fs.walkNode(root, min, max, fn, 0)
	if err == errStop {
		return nil
	}
	return err
}

func (fs *FS) walkNode(addr uint64, min, max Key, fn func(Key, []byte) error, depth int) error {
	if depth > 16 {
		return errors.New("btrfs: tree too deep (corrupt?)")
	}
	n, err := fs.readNode(addr)
	if err != nil {
		return err
	}
	if n.level == 0 {
		for i, k := range n.keys {
			if k.cmp(min) < 0 {
				continue
			}
			if k.cmp(max) > 0 {
				return errStop
			}
			if err := fn(k, n.data[i]); err != nil {
				return err
			}
		}
		return nil
	}
	for i := range n.keys {
		// child i covers [keys[i], keys[i+1])
		if i+1 < len(n.keys) && n.keys[i+1].cmp(min) <= 0 {
			continue
		}
		if n.keys[i].cmp(max) > 0 {
			return errStop
		}
		if err := fs.walkNode(n.ptrs[i], min, max, fn, depth+1); err != nil {
			return err
		}
	}
	return nil
}

// items collects all items of one (objectid, type) in a tree.
func (fs *FS) items(root, objectID uint64, typ uint8, fn func(k Key, data []byte) error) error {
	return fs.walk(root, Key{objectID, typ, 0}, Key{objectID, typ, math.MaxUint64}, fn)
}

// lookup returns the payload of one exact key, or nil.
func (fs *FS) lookup(root uint64, k Key) ([]byte, error) {
	var out []byte
	err := fs.walk(root, k, k, func(_ Key, d []byte) error {
		out = d
		return errStop
	})
	return out, err
}
