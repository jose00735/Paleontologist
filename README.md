# Paleontologist 🦴

**Open the Linux or Windows disk inside a VMware `.vmdk` from Windows — no VM, no drivers, no installs.**

Paleontologist reads **btrfs, ext4/ext3/ext2, NTFS and FAT32/FAT16/FAT12** filesystems straight out of a
`.vmdk` and shows them as a read-only Windows drive letter, in a web browser, or copies files out.
It is a single self-contained executable written in pure Go (no cgo), and it never writes to the disk image.

```
drag Parrot.vmdk onto paleontologist.exe   →   Z:\  appears in Explorer
```

---

## Features

- **Mount as a drive letter** on Windows (through the built-in WebDAV client), read-only.
- **Browse in any web browser** at `http://localhost:PORT/` while mounted.
- **Extract** files or whole folders to a local folder (fastest path, no size limits).
- **Inspect** disks: partitions, filesystems, labels, btrfs subvolumes and snapshots.
- **Drag-and-drop** friendly: dropping a `.vmdk` on the exe mounts it and opens Explorer.
- Picks the **main partition automatically** (the largest readable one, skipping EFI/boot/reserved
  partitions); any other partition is one flag away.
- **Safe on damaged disks:** clear errors for truncated images, and `extract` continues past corrupt files.

## What it reads

| Layer | Supported |
|---|---|
| **VMDK** | monolithicSparse, twoGbMaxExtentSparse, streamOptimized (compressed), monolithicFlat, twoGbMaxExtentFlat, ESXi vmfs / `-flat`, ZERO extents, Workstation snapshot chains (parent `.vmdk` in the same folder), raw `.img` |
| **Partitions** | GPT, MBR (including extended/logical partitions), or no partition table |
| **btrfs** | single device plus mirrored profiles (DUP, RAID1, RAID1C3/4; RAID0/10 when all data is on this disk), all subvolumes and snapshots, inline / regular / preallocated extents, holes, reflinks, zlib, LZO and zstd compression |
| **ext2 / ext3 / ext4** | 1–64 KiB blocks, indirect block maps, extent trees of any depth, 64bit, flex_bg, meta_bg, bigalloc, huge files, inline data, hash-indexed (htree) directories, metadata_csum, sparse and preallocated files, fast and slow symlinks, nanosecond timestamps |
| **NTFS** | NTFS 3.x (Windows XP → 11), any cluster size, fragmented `$MFT`, attribute lists, sparse and LZNT1-compressed files, B-tree directory indexes, symbolic links, junctions, Interix symlinks written by Linux (ntfs-3g) |
| **FAT** | FAT12, FAT16, FAT32, VFAT long file names (Unicode), Windows NT lowercase 8.3 names, 512–4096-byte sectors |

**Not supported:** ESXi delta disks (`vmfsSparse` / `seSparse` — consolidate snapshots first), encrypted VMDKs,
LUKS, BitLocker, LVM, XFS, exFAT, btrfs RAID5/6, multi-device btrfs with data on other disks, fscrypt-encrypted
ext4 folders, and NTFS files that are EFS-encrypted, deduplicated or CompactOS/WOF-compressed. These are detected
and reported with a clear message instead of returning wrong data. Writing is not supported, by design.

## Download

Grab `paleontologist.exe` (x64) or `paleontologist-arm64.exe` (Windows on ARM) from the
[Releases](../../releases) page, or [build it yourself](#building).

## Usage

```
paleontologist.exe disk.vmdk                          mount (same as "mount")
paleontologist.exe info disk.vmdk                     partitions, filesystems, labels, subvolumes
paleontologist.exe mount disk.vmdk -partition 4       mount a specific partition (numbers come from "info")
paleontologist.exe mount disk.vmdk -drive Y           choose the drive letter
paleontologist.exe mount disk.vmdk -subvol @home      use one folder/subvolume as the drive root
paleontologist.exe serve disk.vmdk                    only the browser/WebDAV server, no drive letter
paleontologist.exe ls disk.vmdk /etc                  list a folder
paleontologist.exe extract disk.vmdk /home/jose C:\Rescue
                                                      copy a file or folder out of the disk
paleontologist.exe fix-webdav-limit                   (as Administrator) allow files > 50 MB on the drive
paleontologist.exe version
```

Options can go anywhere on the command line:

| Option | Meaning |
|---|---|
| `-partition N` | use partition `N` (default: the largest readable partition) |
| `-drive X` | drive letter to use (default: the last free letter) |
| `-port P` | local port for the server (default: any free port) |
| `-subvol PATH` | show this folder or btrfs subvolume as the root (e.g. `@home`, `Users`) |

The server only listens on `127.0.0.1`. Press **Enter** (or Ctrl+C) to unmount and exit.

### Example

Output from a test disk (GPT with an EFI partition, NTFS and ext4), shortened:

```
> paleontologist.exe info Windows11.vmdk
Disk: Windows11.vmdk (streamOptimized, 1118 MiB)

Partitions:
  #1       0.1 GiB  fat    EFI system "EFI system partition"
  #2       0.0 GiB  unknown Microsoft reserved "Microsoft reserved partition"
  #3       0.6 GiB  ntfs   ... "Basic data partition"
  #4       0.4 GiB  ext4   ... "linux"

ntfs on partition #3
  label: "BigClusters"
  size:  0.6 GiB   cluster size: 65536   MFT record: 1024
  NTFS version: 3.1
  top level: big/  deep/  empty.txt  hundred.txt  links/  many/  tiny.txt  unicode/

ext4 on partition #4
  label: "fancy"
  size:  0.4 GiB (0.2 GiB used)   block size: 4096   inode size: 512
  top level: big/  deep/  empty.txt  hundred.txt  links/  lost+found/  many/  tiny.txt  unicode/
```

`paleontologist.exe Windows11.vmdk` would mount partition #3 (the largest readable one);
`-partition 4` mounts the ext4 one instead.

## Tips and troubleshooting

- **Files over 50 MB fail to open on the drive letter.** Windows' WebDAV client limits file size to 50 MB by
  default. Run `paleontologist.exe fix-webdav-limit` once from an Administrator terminal (raises it to 4 GB,
  the Windows maximum), or use the browser view or `extract` for big files.
- **"Could not map a drive letter".** The Windows *WebClient* service must be running
  (`services.msc` → WebClient → Start). It ships with Windows 10/11. Paleontologist tries to start it and skips
  letters that are in use or remembered by Windows.
- **First folder listing is slow.** Internet Options → Connections → LAN settings → untick
  "Automatically detect settings" (a long-standing Windows WebDAV quirk).
- **btrfs layouts** (Ubuntu, openSUSE, Parrot…) appear as top-level folders like `@`, `@home` and snapshots.
  Use `-subvol @` to make one of them the root.
- **Symlinks** are followed: Linux symlinks on btrfs/ext, symbolic links and junctions on NTFS, and the
  Interix symlinks that Linux writes on NTFS. Absolute links resolve inside the same disk (`C:\x` → `\x`,
  `/etc` → the subvolume's `/etc`). Broken links, devices, FIFOs and sockets are hidden. `extract` skips
  symlinks and renames names that Windows forbids (`<>:"|?*`, `CON`, `NUL`, …).
- **FAT and NTFS paths are case-insensitive**, like on Windows. NTFS metadata files (`$MFT`, `$Bitmap`, …)
  are hidden, and only each file's main data stream is shown.
- **Shut the VM down first** (or copy the `.vmdk`) so the disk is consistent. ext3/ext4 journals are not
  replayed, so a disk from a crashed VM may miss its last few seconds of changes (`info` warns about it).

## Building

Requires Go 1.24 or newer (see `go.mod`).

```bash
git clone <this repo>
cd paleontologist

# Windows x64
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o paleontologist.exe .

# Windows on ARM
GOOS=windows GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o paleontologist-arm64.exe .

# Linux / macOS (serve, info, ls and extract work; drive letters are Windows-only)
go build -o paleontologist .
```

On Windows PowerShell, set the variables first: `$env:GOOS="windows"; $env:GOARCH="amd64"`.

Dependencies (fetched automatically by `go build`):

- [`github.com/klauspost/compress`](https://github.com/klauspost/compress) — zstd decompression for btrfs
- [`golang.org/x/net/webdav`](https://pkg.go.dev/golang.org/x/net/webdav) — WebDAV server

`go.mod` has a `replace` that fetches `golang.org/x/net` from its GitHub mirror. You can remove it if
`proxy.golang.org` is reachable from your network.

## Project layout

```
paleontologist/
├── main.go             commands, flags, partition selection, extract
├── drive_windows.go    drive-letter mapping (net use), WebClient, Explorer label, 50 MB limit fix
├── drive_other.go      stubs for non-Windows builds
├── vmdk/               VMDK reader: descriptors, sparse/stream-optimized extents, snapshot chains
├── part/               GPT / MBR partition tables and filesystem signature probing
├── vfs/                the small read-only filesystem interface every reader implements,
│                       plus path resolution with symlinks
├── davfs/              read-only WebDAV adapter + HTML listings for browsers
├── btrfs/              superblock, chunk tree, B-trees, subvolumes, extents, zlib/LZO/zstd
├── ext/                superblock, group descriptors, inodes, extent trees, block maps, directories
├── ntfs/               boot sector, MFT + fixups, attributes, runlists, LZNT1, $I30 indexes, reparse points
└── fat/                BPB, FAT12/16/32 chains, directories and VFAT long names
```

Every filesystem implements `vfs.FS`:

```go
type FS interface {
	Type() string
	Label() string
	Root() Node
	Stat(Node) (Stat, error)
	ReadDir(Node) ([]Entry, error)
	Open(Node) (File, error) // io.ReaderAt + Size()
	Readlink(Node) (string, error)
}
```

To add a filesystem, implement it in its own package, detect its signature in `part.Probe`, and add a case to
`openFS` in `main.go`. The drive letter, browser view, `ls` and `extract` then work for it automatically.

## Testing

```bash
go vet ./...
go test ./...
```

The readers were verified against images created by the reference implementations: the Linux kernel
(btrfs, ext2/3/4), `mkfs.btrfs`, `mkntfs` + `ntfs-3g`, `mkfs.fat` + mtools, and qemu's vvfat. Each image was
packed into every VMDK subformat with `qemu-img`, and Paleontologist's `extract` output was compared
byte-for-byte with the original files. They were also fuzzed with randomly corrupted images to make sure
damaged disks give errors rather than crashes or hangs.

To reproduce a check, extract an image and compare it with the source tree:

```bash
./paleontologist extract disk.img / out
diff -r source-tree out/<label>
```

## How it works (short version)

1. `vmdk` turns the `.vmdk` (and its extent and parent files) into a single random-access disk.
2. `part` finds the partitions and probes each one for a filesystem signature.
3. The matching reader parses the filesystem's on-disk structures directly. Nothing is mounted by the OS,
   and no admin rights are needed except for the optional `fix-webdav-limit`.
4. `davfs` serves the tree over WebDAV on localhost, and Windows maps it with
   `net use X: http://localhost:PORT/`.

## How this was made

The idea, requirements and real-world testing are by **Jose Acosta Hernandez**: I needed to get files out
of a btrfs Linux VM disk on Windows and couldn't find a simple tool for it.

Most of the code, the tests and this README were written by **Claude** (Anthropic's AI) working from those
requirements, with me directing what to build, trying it on my own disks and reporting what broke.
Bug reports and pull requests are welcome.

## License

<!-- Choose a license (e.g. MIT or Apache-2.0), add a LICENSE file, and update this section. -->
No license has been chosen yet.
