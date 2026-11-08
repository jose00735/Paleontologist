// Package part finds partitions (MBR with logical partitions, or GPT) on a disk image
// and probes each one for a filesystem signature.
package part

import (
	"encoding/binary"
	"fmt"
	"io"
	"strings"
	"unicode/utf16"
)

// Partition is one region of the disk.
type Partition struct {
	Index int    // 1-based; 0 means "whole disk, no partition table"
	Start int64  // bytes
	Size  int64  // bytes
	Table string // "gpt", "mbr" or "none"
	Type  string // partition type as text
	Name  string // GPT name
	FS    string // detected filesystem ("btrfs", "ext2", "ext3", "ext4", "xfs", "ntfs", "fat", "swap", "lvm", "luks", "")
}

func (p Partition) String() string {
	fs := p.FS
	if fs == "" {
		fs = "unknown"
	}
	name := ""
	if p.Name != "" {
		name = " \"" + p.Name + "\""
	}
	return fmt.Sprintf("#%d  %8.1f GiB  %-6s %s%s", p.Index, float64(p.Size)/(1<<30), fs, p.Type, name)
}

// List returns all partitions with their detected filesystem.
func List(r io.ReaderAt, size int64) ([]Partition, error) {
	mbr := make([]byte, 512)
	if _, err := r.ReadAt(mbr, 0); err != nil {
		return nil, err
	}
	var parts []Partition
	if mbr[510] == 0x55 && mbr[511] == 0xaa {
		isGPT := false
		for i := 0; i < 4; i++ {
			if mbr[446+i*16+4] == 0xee {
				isGPT = true
			}
		}
		if isGPT {
			p, err := listGPT(r, size)
			if err == nil {
				parts = p
			}
		}
		if parts == nil {
			parts = listMBR(r, mbr, size)
		}
	}
	if len(parts) == 0 {
		parts = []Partition{{Index: 0, Start: 0, Size: size, Table: "none", Type: "whole disk"}}
	}
	for i := range parts {
		parts[i].FS = Probe(io.NewSectionReader(r, parts[i].Start, parts[i].Size))
	}
	return parts, nil
}

func listMBR(r io.ReaderAt, mbr []byte, size int64) []Partition {
	var parts []Partition
	idx := 1
	for i := 0; i < 4; i++ {
		e := mbr[446+i*16:]
		typ := e[4]
		lba := int64(binary.LittleEndian.Uint32(e[8:]))
		n := int64(binary.LittleEndian.Uint32(e[12:]))
		if typ == 0 || n == 0 {
			idx++
			continue
		}
		if typ == 0x05 || typ == 0x0f || typ == 0x85 {
			parts = append(parts, logical(r, lba, size)...)
			idx++
			continue
		}
		parts = append(parts, Partition{Index: idx, Start: lba * 512, Size: n * 512, Table: "mbr", Type: mbrType(typ)})
		idx++
	}
	// Number logical partitions from 5, like Linux does.
	l := 5
	for i := range parts {
		if parts[i].Index == -1 {
			parts[i].Index = l
			l++
		}
	}
	return parts
}

func logical(r io.ReaderAt, extStart, size int64) []Partition {
	var out []Partition
	next := extStart
	ebr := make([]byte, 512)
	for guard := 0; guard < 128; guard++ {
		if _, err := r.ReadAt(ebr, next*512); err != nil || ebr[510] != 0x55 || ebr[511] != 0xaa {
			break
		}
		e := ebr[446:]
		if e[4] != 0 {
			lba := next + int64(binary.LittleEndian.Uint32(e[8:]))
			n := int64(binary.LittleEndian.Uint32(e[12:]))
			if lba*512 < size {
				out = append(out, Partition{Index: -1, Start: lba * 512, Size: n * 512, Table: "mbr", Type: mbrType(e[4]) + " (logical)"})
			}
		}
		e2 := ebr[462:]
		rel := int64(binary.LittleEndian.Uint32(e2[8:]))
		if e2[4] == 0 || rel == 0 {
			break
		}
		next = extStart + rel
	}
	return out
}

func mbrType(t byte) string {
	switch t {
	case 0x83:
		return "Linux"
	case 0x82:
		return "Linux swap"
	case 0x8e:
		return "Linux LVM"
	case 0xfd:
		return "Linux RAID"
	case 0x07:
		return "NTFS/exFAT"
	case 0x0b, 0x0c, 0x0e, 0x06:
		return "FAT"
	case 0xef:
		return "EFI system"
	}
	return fmt.Sprintf("type 0x%02x", t)
}

func listGPT(r io.ReaderAt, size int64) ([]Partition, error) {
	h := make([]byte, 512)
	if _, err := r.ReadAt(h, 512); err != nil {
		return nil, err
	}
	if string(h[:8]) != "EFI PART" {
		return nil, fmt.Errorf("no GPT header")
	}
	entLBA := int64(binary.LittleEndian.Uint64(h[72:]))
	num := int(binary.LittleEndian.Uint32(h[80:]))
	esz := int(binary.LittleEndian.Uint32(h[84:]))
	if num <= 0 || num > 1024 || esz < 128 || esz > 4096 {
		return nil, fmt.Errorf("bad GPT geometry")
	}
	buf := make([]byte, num*esz)
	if _, err := r.ReadAt(buf, entLBA*512); err != nil && err != io.EOF {
		return nil, err
	}
	var parts []Partition
	for i := 0; i < num; i++ {
		e := buf[i*esz : (i+1)*esz]
		tg := guid(e[0:16])
		if tg == "00000000-0000-0000-0000-000000000000" {
			continue
		}
		first := int64(binary.LittleEndian.Uint64(e[32:]))
		last := int64(binary.LittleEndian.Uint64(e[40:]))
		u := make([]uint16, 36)
		for j := range u {
			u[j] = binary.LittleEndian.Uint16(e[56+j*2:])
		}
		name := strings.TrimRight(string(utf16.Decode(u)), "\x00")
		parts = append(parts, Partition{Index: i + 1, Start: first * 512, Size: (last - first + 1) * 512, Table: "gpt", Type: gptType(tg), Name: name})
	}
	return parts, nil
}

func guid(b []byte) string {
	return fmt.Sprintf("%08x-%04x-%04x-%x-%x", binary.LittleEndian.Uint32(b[0:]), binary.LittleEndian.Uint16(b[4:]),
		binary.LittleEndian.Uint16(b[6:]), b[8:10], b[10:16])
}

func gptType(g string) string {
	switch strings.ToLower(g) {
	case "0fc63daf-8483-4772-8e79-3d69d8477de4":
		return "Linux filesystem"
	case "c12a7328-f81f-11d2-ba4b-00a0c93ec93b":
		return "EFI system"
	case "21686148-6449-6e6f-744e-656564454649":
		return "BIOS boot"
	case "0657fd6d-a4ab-43c4-84e5-0933c84b4f4f":
		return "Linux swap"
	case "e6d6d379-f507-44c2-a23c-238f2a3df928":
		return "Linux LVM"
	case "ebd0a0a2-b9e5-4433-87c0-68b6b72699c7":
		return "Microsoft basic data"
	case "e3c9e316-0b5c-4db8-817d-f92df00215ae":
		return "Microsoft reserved"
	case "de94bba4-06d1-4d40-a16a-bfd50179d6ac":
		return "Windows recovery"
	case "4f68bce3-e8cd-4db1-96e7-fbcaf984b709":
		return "Linux root (x86-64)"
	case "933ac7e1-2eb4-4f13-b844-0e14e2aef915":
		return "Linux /home"
	case "bc13c2ff-59e6-4262-a352-b275fd6f7172":
		return "Linux /boot (XBOOTLDR)"
	case "ca7d7ccb-63ed-4c53-861c-1742536059cc":
		return "LUKS"
	}
	return "GUID " + g
}

// Probe identifies the filesystem at the start of r.
func Probe(r io.ReaderAt) string {
	b := make([]byte, 8)
	if _, err := r.ReadAt(b, 0x10040); err == nil && string(b) == "_BHRfS_M" {
		return "btrfs"
	}
	s := make([]byte, 4096)
	n, _ := r.ReadAt(s, 0)
	s = s[:n]
	switch {
	case len(s) >= 6 && string(s[:6]) == "LUKS\xba\xbe":
		return "luks"
	case len(s) >= 4 && string(s[:4]) == "XFSB":
		return "xfs"
	case len(s) >= 11 && string(s[3:11]) == "-FVE-FS-":
		return "bitlocker"
	case len(s) >= 11 && string(s[3:11]) == "NTFS    ":
		return "ntfs"
	case len(s) >= 11 && string(s[3:11]) == "EXFAT   ":
		return "exfat"
	case len(s) >= 0x468 && binary.LittleEndian.Uint16(s[0x438:]) == 0xef53:
		le := binary.LittleEndian
		compat, incompat := le.Uint32(s[0x45C:]), le.Uint32(s[0x460:])
		switch {
		case incompat&(0x40|0x80|0x200) != 0: // extents, 64bit, flex_bg
			return "ext4"
		case compat&0x4 != 0: // has_journal
			return "ext3"
		}
		return "ext2"
	case len(s) >= 4096 && strings.HasPrefix(string(s[4086:]), "SWAPSPACE2"):
		return "swap"
	case len(s) >= 520 && string(s[512:520]) == "LABELONE":
		return "lvm"
	case len(s) >= 90 && (string(s[54:59]) == "FAT12" || string(s[54:59]) == "FAT16" || string(s[82:87]) == "FAT32"):
		return "fat"
	case len(s) >= 512 && looksLikeFAT(s):
		return "fat"
	}
	return ""
}

// looksLikeFAT checks for a plausible FAT BIOS parameter block without the type string.
func looksLikeFAT(s []byte) bool {
	le := binary.LittleEndian
	if s[510] != 0x55 || s[511] != 0xAA || (s[0] != 0xEB && s[0] != 0xE9) {
		return false
	}
	bps := le.Uint16(s[11:])
	spc := s[13]
	return (bps == 512 || bps == 1024 || bps == 2048 || bps == 4096) && spc != 0 && spc&(spc-1) == 0 &&
		le.Uint16(s[14:]) != 0 && (s[16] == 1 || s[16] == 2) && s[21] >= 0xF0
}
