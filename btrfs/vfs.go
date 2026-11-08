package btrfs

import (
	"fmt"

	"paleontologist/vfs"
)

// View exposes a btrfs filesystem through the generic vfs interface. Nodes are Ino values.
type View struct{ B *FS }

func (f *FS) View() *View { return &View{f} }

func (v *View) Type() string   { return "btrfs" }
func (v *View) Label() string  { return v.B.sb.Label }
func (v *View) Root() vfs.Node { return RootIno }

func (v *View) Stat(n vfs.Node) (vfs.Stat, error) {
	st, err := v.B.Stat(n.(Ino))
	if err != nil {
		return vfs.Stat{}, err
	}
	return vfs.Stat{Size: st.Size, Mode: st.FileMode(), Mtime: st.Mtime}, nil
}

func (v *View) ReadDir(n vfs.Node) ([]vfs.Entry, error) {
	ents, err := v.B.ReadDir(n.(Ino))
	if err != nil {
		return nil, err
	}
	out := make([]vfs.Entry, len(ents))
	for i, e := range ents {
		k := vfs.KindOther
		switch e.Type {
		case 1:
			k = vfs.KindFile
		case 2:
			k = vfs.KindDir
		case 7:
			k = vfs.KindSymlink
		}
		out[i] = vfs.Entry{Name: e.Name, Node: e.Ino, Kind: k}
		if e.Subvol {
			out[i].Note = "subvolume"
		}
	}
	return out, nil // already sorted
}

func (v *View) Open(n vfs.Node) (vfs.File, error) { return v.B.OpenFile(n.(Ino)) }

func (v *View) Readlink(n vfs.Node) (string, error) { return v.B.Readlink(n.(Ino)) }

func (v *View) Parent(n vfs.Node) (vfs.Node, error) { return v.B.Parent(n.(Ino)) }

// SymlinkRoot: absolute links resolve from the root of the subvolume that holds them.
func (v *View) SymlinkRoot(n vfs.Node) vfs.Node { return Ino{n.(Ino).Tree, FirstFreeID} }

func (v *View) Info() []string {
	sb := v.B.sb
	out := []string{
		fmt.Sprintf("uuid:  %s", sb.UUID()),
		fmt.Sprintf("size:  %.1f GiB (%.1f GiB used)", float64(sb.TotalBytes)/(1<<30), float64(sb.BytesUsed)/(1<<30)),
		fmt.Sprintf("devices: %d   node size: %d   sector size: %d", sb.NumDevices, sb.NodeSize, sb.SectorSize),
	}
	if subs, err := v.B.Subvolumes(); err == nil && len(subs) > 0 {
		out = append(out, "subvolumes / snapshots:")
		for _, s := range subs {
			out = append(out, fmt.Sprintf("  id %-5d %s", s.ID, s.Path))
		}
	}
	return out
}
