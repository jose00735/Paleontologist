package ntfs

import (
	"encoding/binary"
	"errors"
	"io"
	"paleontologist/vfs"
	"sort"
	"sync"
)

// stream reads the value of an attribute (resident, non-resident, sparse or compressed).
type stream struct {
	fs *FS
	a  attr

	mu       sync.Mutex
	lastUnit int64
	lastData []byte
}

func (s *stream) Size() int64 { return s.a.size }

// lookup returns the run containing vcn, or nil.
func (s *stream) lookup(vcn uint64) *nrun {
	r := s.a.runs
	i := sort.Search(len(r), func(i int) bool { return r[i].vcn > vcn }) - 1
	if i >= 0 && vcn < r[i].vcn+r[i].n {
		return &r[i]
	}
	return nil
}

func (s *stream) ReadAt(p []byte, off int64) (int, error) {
	size := s.a.size
	if off >= size {
		return 0, io.EOF
	}
	want := len(p)
	var eof error
	if off+int64(want) > size {
		want = int(size - off)
		eof = io.EOF
	}
	p = p[:want]
	if !s.a.nonres {
		n := copy(p, s.a.value[min(off, int64(len(s.a.value))):])
		clear(p[n:])
		return want, eof
	}
	// bytes past the initialized size read as zeros
	valid := want
	if init := s.a.initSize; off+int64(valid) > init {
		valid = int(max(0, init-off))
		clear(p[valid:])
	}
	var err error
	if s.a.flags&flagCompressed != 0 && s.a.cu > 0 {
		err = s.readCompressed(p[:valid], off)
	} else {
		err = s.readPlain(p[:valid], off)
	}
	if err != nil {
		return 0, err
	}
	return want, eof
}

func (s *stream) readPlain(p []byte, off int64) error {
	cs := s.fs.clusterSize
	done := 0
	for done < len(p) {
		cur := off + int64(done)
		vcn := uint64(cur / cs)
		r := s.lookup(vcn)
		var n int
		if r == nil {
			// past the runlist or between runs: zeros up to the next run
			n = len(p) - done
			next := sort.Search(len(s.a.runs), func(i int) bool { return s.a.runs[i].vcn > vcn })
			if next < len(s.a.runs) {
				if lim := int64(s.a.runs[next].vcn)*cs - cur; lim < int64(n) {
					n = int(lim)
				}
			}
			clear(p[done : done+n])
		} else {
			runEnd := int64(r.vcn+r.n) * cs
			n = len(p) - done
			if int64(n) > runEnd-cur {
				n = int(runEnd - cur)
			}
			if r.sparse {
				clear(p[done : done+n])
			} else {
				phys := int64(r.lcn)*cs + (cur - int64(r.vcn)*cs)
				if err := vfs.ReadFull(s.fs.dev, p[done:done+n], phys); err != nil {
					return err
				}
			}
		}
		done += n
	}
	return nil
}

func (s *stream) readCompressed(p []byte, off int64) error {
	cs := s.fs.clusterSize
	unitClusters := int64(1) << s.a.cu
	unitSize := unitClusters * cs
	done := 0
	for done < len(p) {
		cur := off + int64(done)
		u := cur / unitSize
		data, err := s.unit(u, unitClusters, unitSize)
		if err != nil {
			return err
		}
		in := cur - u*unitSize
		n := copy(p[done:], data[in:])
		done += n
	}
	return nil
}

// unit returns the decompressed contents of compression unit u.
func (s *stream) unit(u, unitClusters, unitSize int64) ([]byte, error) {
	s.mu.Lock()
	if s.lastData != nil && s.lastUnit == u {
		d := s.lastData
		s.mu.Unlock()
		return d, nil
	}
	s.mu.Unlock()
	cs := s.fs.clusterSize
	first := uint64(u * unitClusters)
	allocated := int64(0)
	for c := int64(0); c < unitClusters; c++ {
		if r := s.lookup(first + uint64(c)); r != nil && !r.sparse {
			allocated++
		}
	}
	out := make([]byte, unitSize)
	switch {
	case allocated == 0:
		// sparse unit
	case allocated == unitClusters:
		if err := s.readPlain(out, u*unitSize); err != nil {
			return nil, err
		}
	default:
		raw := make([]byte, allocated*cs)
		if err := s.readPlain(raw, u*unitSize); err != nil {
			return nil, err
		}
		if err := lznt1(raw, out); err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	s.lastUnit, s.lastData = u, out
	s.mu.Unlock()
	return out, nil
}

// lznt1 decompresses src into dst (dst's length is the expected uncompressed size).
func lznt1(src, dst []byte) (err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("ntfs: corrupt compressed data")
		}
	}()
	le := binary.LittleEndian
	sp, dp := 0, 0
	for sp+2 <= len(src) && dp < len(dst) {
		h := le.Uint16(src[sp:])
		if h == 0 {
			break
		}
		clen := int(h&0xFFF) + 1
		sp += 2
		end := sp + clen
		if end > len(src) {
			end = len(src)
		}
		chunkStart := dp
		if h&0x8000 == 0 { // stored chunk
			n := copy(dst[dp:], src[sp:end])
			dp += n
			sp = end
			continue
		}
		for sp < end && dp < len(dst) {
			flags := src[sp]
			sp++
			for bit := 0; bit < 8 && sp < end && dp < len(dst); bit++ {
				if flags&(1<<bit) == 0 {
					dst[dp] = src[sp]
					dp++
					sp++
					continue
				}
				if sp+2 > end {
					sp = end
					break
				}
				tok := int(le.Uint16(src[sp:]))
				sp += 2
				pos := dp - chunkStart
				lmask, oshift := 0xFFF, 12
				for i := pos - 1; i >= 0x10; i >>= 1 {
					lmask >>= 1
					oshift--
				}
				offset := (tok >> oshift) + 1
				length := (tok & lmask) + 3
				from := dp - offset
				if from < chunkStart {
					return errors.New("ntfs: bad LZNT1 back-reference")
				}
				for k := 0; k < length && dp < len(dst); k++ {
					dst[dp] = dst[from+k]
					dp++
				}
			}
		}
		sp = end
		// a compressed chunk that ends early leaves zeros up to 4 KiB
		if dp-chunkStart < 4096 && dp < len(dst) && sp < len(src) {
			pad := min(4096-(dp-chunkStart), len(dst)-dp)
			dp += pad
		}
	}
	return nil
}
