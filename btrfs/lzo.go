package btrfs

import (
	"errors"
	"fmt"
)

var errLZO = errors.New("lzo: corrupt input")

// lzo1xDecompress is a straightforward port of the LZO1X decompressor (lzo1x_decompress_safe).
func lzo1xDecompress(in []byte, hint int) (out []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			out, err = nil, fmt.Errorf("%w (%v)", errLZO, r)
		}
	}()
	out = make([]byte, 0, hint)
	if len(in) == 0 {
		return nil, errLZO
	}
	ip := 0
	t := 0
	lit := func(n int) {
		out = append(out, in[ip:ip+n]...)
		ip += n
	}
	match := func(pos, n int) {
		if pos < 0 || pos >= len(out) {
			panic("match before start of output")
		}
		for i := 0; i < n; i++ { // byte by byte: source may overlap destination
			out = append(out, out[pos+i])
		}
	}
	zeroRun := func(base int) int {
		for in[ip] == 0 {
			t += 255
			ip++
			if t > 1<<30 {
				panic("run too long")
			}
		}
		t += base + int(in[ip])
		ip++
		return t
	}

	const (
		loopTop = iota
		firstLiteralRun
		matchState
		matchDone
		matchNext
	)
	state := loopTop
	if in[0] > 17 {
		t = int(in[0]) - 17
		ip = 1
		if t < 4 {
			state = matchNext
		} else {
			lit(t)
			state = firstLiteralRun
		}
	}
	for {
		switch state {
		case loopTop:
			t = int(in[ip])
			ip++
			if t >= 16 {
				state = matchState
				continue
			}
			if t == 0 {
				zeroRun(15)
			}
			lit(t + 3)
			state = firstLiteralRun
		case firstLiteralRun:
			t = int(in[ip])
			ip++
			if t >= 16 {
				state = matchState
				continue
			}
			pos := len(out) - (1 + 0x0800) - (t >> 2) - int(in[ip])<<2
			ip++
			match(pos, 3)
			state = matchDone
		case matchState:
			switch {
			case t >= 64: // M2
				pos := len(out) - 1 - ((t >> 2) & 7) - int(in[ip])<<3
				ip++
				t = (t >> 5) - 1
				match(pos, t+2)
			case t >= 32: // M3
				t &= 31
				if t == 0 {
					zeroRun(31)
				}
				pos := len(out) - 1 - (int(in[ip])>>2 + int(in[ip+1])<<6)
				ip += 2
				match(pos, t+2)
			case t >= 16: // M4
				pos := len(out) - (t&8)<<11
				t &= 7
				if t == 0 {
					zeroRun(7)
				}
				pos -= int(in[ip])>>2 + int(in[ip+1])<<6
				ip += 2
				if pos == len(out) {
					return out, nil // end of stream marker
				}
				pos -= 0x4000
				match(pos, t+2)
			default: // M1
				pos := len(out) - 1 - (t >> 2) - int(in[ip])<<2
				ip++
				match(pos, 2)
			}
			state = matchDone
		case matchDone:
			t = int(in[ip-2]) & 3
			if t == 0 {
				state = loopTop
			} else {
				state = matchNext
			}
		case matchNext:
			lit(t)
			t = int(in[ip])
			ip++
			state = matchState
		}
	}
}
