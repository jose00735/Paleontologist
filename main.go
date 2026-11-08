// Paleontologist digs btrfs, ext2/3/4, NTFS and FAT filesystems out of VMware .vmdk files and lets you browse it
// from Windows as a read-only drive letter (WebDAV), from a web browser, or by extracting files.
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"paleontologist/btrfs"
	"paleontologist/davfs"
	"paleontologist/ext"
	"paleontologist/fat"
	"paleontologist/ntfs"
	"paleontologist/part"
	"paleontologist/vfs"
	"paleontologist/vmdk"
)

const version = "1.1.0"

func usage() {
	fmt.Fprintf(os.Stderr, `Paleontologist %s — read btrfs, ext2/3/4, NTFS and FAT disks inside VMware .vmdk files (read-only)

USAGE
  paleontologist <disk.vmdk>                      mount as a Windows drive (same as "mount")
  paleontologist mount <disk.vmdk> [options]      mount as a drive letter and open Explorer
  paleontologist serve <disk.vmdk> [options]      only start the web server (browser / WebDAV)
  paleontologist info <disk.vmdk>                 show disk, partitions, filesystems, subvolumes
  paleontologist ls <disk.vmdk> [path]            list a folder inside the disk
  paleontologist extract <disk.vmdk> <path> <destination folder>
                                                  copy a file or folder out of the disk
  paleontologist fix-webdav-limit                 (Windows, run as administrator) allow files
                                                  bigger than 50 MB through the mapped drive

OPTIONS
  -partition N   use partition N (default: the largest readable partition)
  -drive X       drive letter to use (default: last free letter, e.g. Z)
  -port P        local port (default: any free port)
  -subvol PATH   show this folder/subvolume as the drive root (e.g. "@home" or "Users")

You can also drag a .vmdk file onto paleontologist.exe.
`, version)
}

func main() {
	log.SetFlags(0)
	args := os.Args[1:]
	interactive := false
	if len(args) == 0 {
		if runtime.GOOS != "windows" {
			usage()
			os.Exit(2)
		}
		// double-clicked: ask for the file
		interactive = true
		fmt.Printf("Paleontologist %s\n\nDrag your .vmdk file onto this window (or type its path) and press Enter:\n> ", version)
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		line = strings.Trim(strings.TrimSpace(line), `"'& `)
		if line == "" {
			return
		}
		args = []string{"mount", line}
	}
	cmd := args[0]
	switch cmd {
	case "mount", "serve", "info", "ls", "extract", "fix-webdav-limit", "help", "-h", "--help", "version":
		args = args[1:]
	default:
		cmd = "mount"
		if strings.HasPrefix(args[0], "-") {
			usage()
			os.Exit(2)
		}
		interactive = interactive || runtime.GOOS == "windows" && len(os.Args) == 2
	}
	var err error
	switch cmd {
	case "help", "-h", "--help":
		usage()
		return
	case "version":
		fmt.Println(version)
		return
	case "fix-webdav-limit":
		err = fixWebDAVLimit()
	case "info":
		err = cmdInfo(args)
	case "ls":
		err = cmdLs(args)
	case "extract":
		err = cmdExtract(args)
	case "mount", "serve":
		err = cmdMount(args, cmd == "mount")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "\nERROR:", err)
		if interactive {
			fmt.Fprintln(os.Stderr, "\nPress Enter to close.")
			bufio.NewReader(os.Stdin).ReadString('\n')
		}
		os.Exit(1)
	}
}

// parse separates flags from positional args (flags may come after the file name).
func parse(args []string) (*flag.FlagSet, []string, *opts) {
	o := &opts{}
	fs := flag.NewFlagSet("paleontologist", flag.ExitOnError)
	fs.Usage = usage
	fs.IntVar(&o.partition, "partition", 0, "")
	fs.StringVar(&o.drive, "drive", "", "")
	fs.IntVar(&o.port, "port", 0, "")
	fs.StringVar(&o.subvol, "subvol", "", "")
	var pos []string
	for len(args) > 0 {
		fs.Parse(args)
		rest := fs.Args()
		if len(rest) == 0 {
			break
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
	return fs, pos, o
}

type opts struct {
	partition int
	drive     string
	port      int
	subvol    string
}

type opened struct {
	disk  vmdk.Disk
	parts []part.Partition
	p     part.Partition
	fs    vfs.FS
	root  vfs.Node
}

// supported reports whether Paleontologist can read a detected filesystem.
func supported(fsType string) bool {
	switch fsType {
	case "btrfs", "ext2", "ext3", "ext4", "ntfs", "fat":
		return true
	}
	return false
}

// openFS opens the filesystem of a partition.
func openFS(d io.ReaderAt, p part.Partition) (vfs.FS, error) {
	r := io.NewSectionReader(d, p.Start, p.Size)
	switch p.FS {
	case "btrfs":
		b, err := btrfs.Open(r)
		if err != nil {
			return nil, err
		}
		return b.View(), nil
	case "ext2", "ext3", "ext4":
		return ext.Open(r)
	case "ntfs":
		return ntfs.Open(r)
	case "fat":
		return fat.Open(r)
	case "exfat":
		return nil, errors.New("exFAT is not supported yet")
	case "lvm":
		return nil, errors.New("this partition is an LVM volume group; filesystems inside LVM are not supported yet")
	case "luks":
		return nil, errors.New("this partition is encrypted with LUKS; it can't be read without unlocking it in Linux")
	case "bitlocker":
		return nil, errors.New("this partition is encrypted with BitLocker; unlock it in Windows (or decrypt it) first")
	case "xfs":
		return nil, errors.New("XFS is not supported")
	case "swap":
		return nil, errors.New("this is a swap partition (no files)")
	}
	return nil, errors.New("no supported filesystem found on this partition")
}

func openAll(file string, o *opts) (*opened, error) {
	d, err := vmdk.Open(file)
	if err != nil {
		return nil, err
	}
	parts, err := part.List(d, d.Size())
	if err != nil {
		d.Close()
		return nil, err
	}
	res := &opened{disk: d, parts: parts}
	var chosen *part.Partition
	for i := range parts {
		if o.partition != 0 {
			if parts[i].Index == o.partition {
				chosen = &parts[i]
				break
			}
			continue
		}
		// default: the largest readable partition (skips EFI / "System Reserved" / boot partitions)
		if supported(parts[i].FS) && (chosen == nil || parts[i].Size > chosen.Size) {
			chosen = &parts[i]
		}
	}
	if chosen == nil {
		var b strings.Builder
		for _, p := range parts {
			b.WriteString("\n  " + p.String())
		}
		hint := ""
		for _, p := range parts {
			switch p.FS {
			case "lvm":
				hint = "\nThe disk uses LVM; filesystems inside LVM volumes are not supported yet."
			case "luks":
				hint = "\nA partition is encrypted with LUKS; it can't be read without unlocking it in Linux."
			case "bitlocker":
				hint = "\nA partition is encrypted with BitLocker; it can't be read without unlocking it in Windows."
			}
		}
		d.Close()
		if o.partition != 0 {
			return nil, fmt.Errorf("partition %d not found. Partitions:%s", o.partition, b.String())
		}
		return nil, fmt.Errorf("no readable partition found (btrfs, ext2/3/4, NTFS or FAT). Partitions:%s%s", b.String(), hint)
	}
	res.p = *chosen
	res.fs, err = openFS(d, *chosen)
	if err != nil {
		d.Close()
		return nil, fmt.Errorf("partition %d: %w", chosen.Index, err)
	}
	res.root = res.fs.Root()
	if o.subvol != "" {
		r, err := vfs.Resolve(res.fs, res.root, strings.Trim(filepath.ToSlash(o.subvol), "/"), true)
		if err != nil {
			d.Close()
			return nil, fmt.Errorf("folder/subvolume %q not found", o.subvol)
		}
		res.root = r
	}
	return res, nil
}

func (o *opened) label() string {
	if l := strings.TrimSpace(o.fs.Label()); l != "" {
		return l
	}
	return o.fs.Type()
}

func cmdInfo(args []string) error {
	_, pos, o := parse(args)
	if len(pos) < 1 {
		return errors.New("usage: paleontologist info <disk.vmdk>")
	}
	d, err := vmdk.Open(pos[0])
	if err != nil {
		return err
	}
	defer d.Close()
	fmt.Println("Disk:", d.Describe())
	parts, err := part.List(d, d.Size())
	if err != nil {
		return err
	}
	fmt.Println("\nPartitions:")
	for _, p := range parts {
		fmt.Println("  " + p.String())
	}
	for _, p := range parts {
		if !supported(p.FS) || (o.partition != 0 && p.Index != o.partition) {
			continue
		}
		f, err := openFS(d, p)
		if err != nil {
			fmt.Printf("\n%s on #%d: %v\n", p.FS, p.Index, err)
			continue
		}
		fmt.Printf("\n%s on partition #%d\n  label: %q\n", f.Type(), p.Index, f.Label())
		if in, ok := f.(vfs.Infoer); ok {
			for _, l := range in.Info() {
				fmt.Println("  " + l)
			}
		}
		ents, err := f.ReadDir(f.Root())
		if err == nil {
			names := []string{}
			for _, e := range ents {
				n := e.Name
				if e.Kind == vfs.KindDir {
					n += "/"
				}
				names = append(names, n)
			}
			if len(names) > 40 {
				names = append(names[:40], fmt.Sprintf("... (%d more)", len(names)-40))
			}
			fmt.Println("  top level:", strings.Join(names, "  "))
		} else {
			fmt.Println("  top level: error:", err)
		}
	}
	return nil
}

func cmdLs(args []string) error {
	_, pos, o := parse(args)
	if len(pos) < 1 {
		return errors.New("usage: paleontologist ls <disk.vmdk> [path]")
	}
	op, err := openAll(pos[0], o)
	if err != nil {
		return err
	}
	defer op.disk.Close()
	p := ""
	if len(pos) > 1 {
		p = strings.Trim(filepath.ToSlash(pos[1]), "/")
	}
	f := op.fs
	n, err := vfs.Resolve(f, op.root, p, true)
	if err != nil {
		return fmt.Errorf("%s: not found", "/"+p)
	}
	st, err := f.Stat(n)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		fmt.Printf("%s %12d %s %s\n", st.Mode, st.Size, st.Mtime.Format("2006-01-02 15:04"), "/"+p)
		return nil
	}
	ents, err := f.ReadDir(n)
	if err != nil {
		return err
	}
	for _, e := range ents {
		s, err := f.Stat(e.Node)
		if err != nil {
			fmt.Printf("%-10s %12s %16s %s\n", "?", "?", "", e.Name)
			continue
		}
		name := e.Name
		switch {
		case s.IsSymlink():
			t, _ := f.Readlink(e.Node)
			name += " -> " + t
		case s.IsDir():
			name += "/"
			if e.Note != "" {
				name += "   [" + e.Note + "]"
			}
		}
		size := fmt.Sprint(s.Size)
		if s.IsDir() {
			size = "-"
		}
		fmt.Printf("%-10s %12s %16s %s\n", s.Mode, size, s.Mtime.Format("2006-01-02 15:04"), name)
	}
	return nil
}

func cmdMount(args []string, mapDrive bool) error {
	_, pos, o := parse(args)
	if len(pos) < 1 {
		return errors.New("usage: paleontologist mount <disk.vmdk>")
	}
	fmt.Println("Opening", pos[0], "...")
	op, err := openAll(pos[0], o)
	if err != nil {
		return err
	}
	defer op.disk.Close()
	fmt.Println("Disk:", op.disk.Describe())
	fmt.Printf("Using partition %s\n%s, label %q\n", op.p.String(), op.fs.Type(), op.fs.Label())
	others := 0
	for _, p := range op.parts {
		if p.Index != op.p.Index && supported(p.FS) {
			others++
		}
	}
	if others > 0 {
		fmt.Println("(other readable partitions exist; pick one with -partition N, see \"info\")")
	}

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", o.port))
	if err != nil {
		return err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	h := davfs.Handler(&davfs.FS{V: op.fs, Root: op.root}, op.label(), log.Printf)
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 30 * time.Second}
	go srv.Serve(ln)
	url := fmt.Sprintf("http://localhost:%d/", port)
	fmt.Println("\nBrowser view:", url)

	letter := ""
	if mapDrive && runtime.GOOS == "windows" {
		letter, err = mapDriveLetter(o.drive, port, op.label())
		if err != nil {
			fmt.Println("\nCould not map a drive letter:", err)
			fmt.Println("You can still browse and download files at", url)
		} else {
			fmt.Printf("\nMounted as %s:  (read-only)\n", letter)
			openExplorer(letter + `:\`)
			fmt.Println("Note: by default Windows refuses files over 50 MB on WebDAV drives.")
			fmt.Println("      Run \"paleontologist fix-webdav-limit\" once as administrator, or use the")
			fmt.Println("      browser link / the extract command for big files.")
		}
	} else if mapDrive {
		fmt.Println("(drive letters are only available on Windows; serving over HTTP/WebDAV)")
	}
	fmt.Println("\nPress Enter (or Ctrl+C) to unmount and exit.")

	done := make(chan struct{}, 2)
	go func() {
		// a real Enter key unmounts; a closed stdin (run in the background) just waits for Ctrl+C
		if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err == nil {
			done <- struct{}{}
		}
	}()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	go func() { <-sig; done <- struct{}{} }()
	<-done
	if letter != "" {
		unmapDriveLetter(letter)
	}
	srv.Close()
	fmt.Println("Unmounted. Bye!")
	return nil
}

func cmdExtract(args []string) error {
	_, pos, o := parse(args)
	if len(pos) < 3 {
		return errors.New("usage: paleontologist extract <disk.vmdk> <path inside disk> <destination folder>")
	}
	op, err := openAll(pos[0], o)
	if err != nil {
		return err
	}
	defer op.disk.Close()
	src := strings.Trim(filepath.ToSlash(pos[1]), "/")
	ino, err := vfs.Resolve(op.fs, op.root, src, true)
	if err != nil {
		return fmt.Errorf("/%s: not found", src)
	}
	dest := pos[2]
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	name := path.Base("/" + src)
	if src == "" {
		name = op.label()
	}
	x := &extractor{fs: op.fs, start: time.Now()}
	err = x.copy(ino, filepath.Join(dest, safeName(name)), 0)
	fmt.Printf("\nDone: %d files, %d folders, %.1f MB in %s", x.files, x.dirs, float64(x.bytes)/(1<<20), time.Since(x.start).Round(time.Second))
	if x.skipped > 0 {
		fmt.Printf(", %d symlinks/special files skipped", x.skipped)
	}
	if x.renamed > 0 {
		fmt.Printf(", %d names changed to be valid on Windows", x.renamed)
	}
	if x.failed > 0 {
		fmt.Printf(", %d errors", x.failed)
	}
	fmt.Println()
	return err
}

type extractor struct {
	fs                                    vfs.FS
	files, dirs, skipped, renamed, failed int
	bytes                                 int64
	start, last                           time.Time
}

func (x *extractor) copy(ino vfs.Node, dst string, depth int) (err error) {
	if depth > 256 {
		return nil
	}
	// a damaged filesystem must not stop the whole rescue: report this item and continue
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("damaged data (%v)", r)
		}
	}()
	st, err := x.fs.Stat(ino)
	if err != nil {
		return err
	}
	switch {
	case st.IsDir():
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return err
		}
		x.dirs++
		ents, err := x.fs.ReadDir(ino)
		if err != nil {
			return err
		}
		for _, e := range ents {
			k := e.Kind
			if k == vfs.KindUnknown {
				if s, err := x.fs.Stat(e.Node); err == nil {
					switch {
					case s.IsDir():
						k = vfs.KindDir
					case s.IsRegular():
						k = vfs.KindFile
					}
				}
			}
			if k != vfs.KindFile && k != vfs.KindDir {
				x.skipped++
				continue
			}
			n := safeName(e.Name)
			if n != e.Name {
				x.renamed++
			}
			if err := x.copy(e.Node, filepath.Join(dst, n), depth+1); err != nil {
				x.failed++
				fmt.Fprintf(os.Stderr, "\n  %s: %v", filepath.Join(dst, n), err)
			}
		}
		os.Chtimes(dst, st.Mtime, st.Mtime)
	case st.IsRegular():
		f, err := x.fs.Open(ino)
		if err != nil {
			return err
		}
		out, err := os.Create(dst)
		if err != nil {
			return err
		}
		n, err := io.Copy(out, io.NewSectionReader(f, 0, f.Size()))
		cerr := out.Close()
		if err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
		os.Chtimes(dst, st.Mtime, st.Mtime)
		x.files++
		x.bytes += n
		if time.Since(x.last) > 500*time.Millisecond {
			x.last = time.Now()
			fmt.Printf("\r%d files, %.1f MB ...", x.files, float64(x.bytes)/(1<<20))
		}
	default:
		x.skipped++
	}
	return nil
}

// safeName makes a Linux file name valid on Windows when running there.
func safeName(n string) string {
	if runtime.GOOS != "windows" {
		return n
	}
	b := []rune(n)
	for i, r := range b {
		if r < 32 || strings.ContainsRune(`<>:"/\|?*`, r) {
			b[i] = '_'
		}
	}
	s := strings.TrimRight(string(b), ". ")
	if s == "" {
		s = "_"
	}
	base := strings.ToUpper(strings.SplitN(s, ".", 2)[0])
	switch base {
	case "CON", "PRN", "AUX", "NUL", "COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
		"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9":
		s = "_" + s
	}
	return s
}
