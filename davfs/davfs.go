// Package davfs exposes a filesystem read-only through WebDAV (for Windows drive
// mapping) and plain HTML directory listings (for web browsers).
package davfs

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"golang.org/x/net/webdav"

	"paleontologist/vfs"
)

// FS adapts a vfs.FS to webdav.FileSystem.
type FS struct {
	V    vfs.FS
	Root vfs.Node
}

var errRO = &os.PathError{Op: "write", Path: "", Err: errors.New("read-only filesystem")}

func (f *FS) Mkdir(ctx context.Context, name string, perm os.FileMode) error { return errRO }
func (f *FS) RemoveAll(ctx context.Context, name string) error               { return errRO }
func (f *FS) Rename(ctx context.Context, oldName, newName string) error      { return errRO }

func clean(name string) string {
	return strings.Trim(path.Clean("/"+name), "/")
}

func (f *FS) resolve(name string) (vfs.Node, vfs.Stat, error) {
	ino, err := vfs.Resolve(f.V, f.Root, clean(name), true)
	if err != nil {
		return nil, vfs.Stat{}, os.ErrNotExist
	}
	st, err := f.V.Stat(ino)
	if err != nil {
		return nil, vfs.Stat{}, os.ErrNotExist
	}
	return ino, st, nil
}

// Stat implements webdav.FileSystem.
func (f *FS) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	_, st, err := f.resolve(name)
	if err != nil {
		return nil, err
	}
	n := path.Base("/" + clean(name))
	if clean(name) == "" {
		n = "/"
	}
	return info(n, st), nil
}

// OpenFile implements webdav.FileSystem (read-only).
func (f *FS) OpenFile(ctx context.Context, name string, flag int, perm os.FileMode) (webdav.File, error) {
	if flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_TRUNC|os.O_APPEND) != 0 {
		return nil, errRO
	}
	ino, st, err := f.resolve(name)
	if err != nil {
		return nil, err
	}
	n := path.Base("/" + clean(name))
	h := &handle{fs: f, ino: ino, st: st, name: n}
	if !st.IsDir() {
		if !st.IsRegular() {
			return nil, os.ErrNotExist
		}
		bf, err := f.V.Open(ino)
		if err != nil {
			return nil, err
		}
		h.file = bf
	}
	return h, nil
}

// List returns the visible entries of a directory (symlinks resolved, specials hidden).
func (f *FS) List(dir vfs.Node) ([]os.FileInfo, error) {
	ents, err := f.V.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]os.FileInfo, 0, len(ents))
	for _, e := range ents {
		ino := e.Node
		kind := e.Kind
		if kind == vfs.KindUnknown {
			if st, err := f.V.Stat(ino); err == nil && st.IsSymlink() {
				kind = vfs.KindSymlink
			}
		}
		if kind == vfs.KindSymlink {
			t, err := vfs.Resolve(f.V, dir, e.Name, true)
			if err != nil {
				continue // dangling symlink
			}
			ino = t
		} else if kind == vfs.KindOther {
			continue // devices, fifos, sockets
		}
		st, err := f.V.Stat(ino)
		if err != nil || (!st.IsDir() && !st.IsRegular()) {
			continue
		}
		out = append(out, info(e.Name, st))
	}
	return out, nil
}

type handle struct {
	fs   *FS
	ino  vfs.Node
	st   vfs.Stat
	name string
	file vfs.File
	pos  int64
	dir  []os.FileInfo
	dpos int
}

func (h *handle) Close() error                { return nil }
func (h *handle) Write(p []byte) (int, error) { return 0, errRO }
func (h *handle) Stat() (os.FileInfo, error)  { return info(h.name, h.st), nil }

func (h *handle) Read(p []byte) (int, error) {
	if h.file == nil {
		return 0, errors.New("is a directory")
	}
	n, err := h.file.ReadAt(p, h.pos)
	h.pos += int64(n)
	if err == io.EOF && n > 0 {
		err = nil
	}
	return n, err
}

func (h *handle) ReadAt(p []byte, off int64) (int, error) {
	if h.file == nil {
		return 0, errors.New("is a directory")
	}
	return h.file.ReadAt(p, off)
}

func (h *handle) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		off += h.pos
	case io.SeekEnd:
		off += h.st.Size
	}
	if off < 0 {
		return 0, errors.New("negative position")
	}
	h.pos = off
	return off, nil
}

func (h *handle) Readdir(count int) ([]os.FileInfo, error) {
	if !h.st.IsDir() {
		return nil, errors.New("not a directory")
	}
	if h.dir == nil {
		l, err := h.fs.List(h.ino)
		if err != nil {
			return nil, err
		}
		h.dir = l
	}
	rest := h.dir[h.dpos:]
	if count <= 0 {
		h.dpos = len(h.dir)
		return rest, nil
	}
	if len(rest) == 0 {
		return nil, io.EOF
	}
	if count > len(rest) {
		count = len(rest)
	}
	h.dpos += count
	return rest[:count], nil
}

type fileInfo struct {
	name  string
	size  int64
	mode  fs.FileMode
	mtime time.Time
}

func info(name string, st vfs.Stat) *fileInfo {
	fi := &fileInfo{name: name, size: st.Size, mtime: st.Mtime}
	if st.IsDir() {
		fi.size = 0
		fi.mode = fs.ModeDir | 0o555
	} else {
		fi.mode = 0o444
	}
	return fi
}

func (i *fileInfo) Name() string       { return i.name }
func (i *fileInfo) Size() int64        { return i.size }
func (i *fileInfo) Mode() fs.FileMode  { return i.mode }
func (i *fileInfo) ModTime() time.Time { return i.mtime }
func (i *fileInfo) IsDir() bool        { return i.mode.IsDir() }
func (i *fileInfo) Sys() any           { return nil }

// ContentType avoids webdav sniffing every file during directory listings.
func (i *fileInfo) ContentType(ctx context.Context) (string, error) {
	if t := mime.TypeByExtension(path.Ext(i.name)); t != "" {
		return t, nil
	}
	return "application/octet-stream", nil
}

// Handler serves WebDAV (read-only) plus HTML listings for browsers.
func Handler(f *FS, label string, logf func(string, ...any)) http.Handler {
	dav := &webdav.Handler{FileSystem: f, LockSystem: webdav.NewMemLS(), Logger: func(r *http.Request, err error) {
		if err != nil && logf != nil && !errors.Is(err, os.ErrNotExist) && err != errRO {
			logf("webdav %s %s: %v", r.Method, r.URL.Path, err)
		}
	}}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "PUT", "DELETE", "MKCOL", "MOVE", "COPY", "PROPPATCH":
			http.Error(w, "read-only", http.StatusForbidden)
			return
		}
		if (r.Method == "GET" || r.Method == "HEAD") && !strings.Contains(r.UserAgent(), "WebDAV") {
			name := r.URL.Path
			if ino, st, err := f.resolve(name); err == nil && st.IsDir() {
				if !strings.HasSuffix(r.URL.Path, "/") {
					http.Redirect(w, r, r.URL.Path+"/", http.StatusMovedPermanently)
					return
				}
				listing(w, f, ino, r.URL.Path, label)
				return
			}
		}
		dav.ServeHTTP(w, r)
	})
}

func listing(w http.ResponseWriter, f *FS, ino vfs.Node, p, label string) {
	ents, err := f.List(ino)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	sort.Slice(ents, func(i, j int) bool {
		if ents[i].IsDir() != ents[j].IsDir() {
			return ents[i].IsDir()
		}
		return strings.ToLower(ents[i].Name()) < strings.ToLower(ents[j].Name())
	})
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>%s</title>
<style>:root{color-scheme:light dark}body{font-family:system-ui,Segoe UI,sans-serif;margin:16px;max-width:1000px;background:Canvas;color:CanvasText}
h1{font-size:18px}table{border-collapse:collapse;width:100%%}td{padding:4px 8px;border-bottom:1px solid #8884}td.s,td.t{text-align:right;white-space:nowrap;opacity:.75}a{text-decoration:none}</style></head><body>
<h1>🦴 %s — %s</h1><table>`, html.EscapeString(label), html.EscapeString(label), html.EscapeString(p))
	if p != "/" {
		fmt.Fprint(w, `<tr><td><a href="../">⬆ ..</a></td><td></td><td></td></tr>`)
	}
	for _, e := range ents {
		n := e.Name()
		href := url.PathEscape(n)
		icon, size := "📄", humanSize(e.Size())
		if e.IsDir() {
			href += "/"
			icon, size = "📁", ""
		}
		fmt.Fprintf(w, `<tr><td>%s <a href="%s">%s</a></td><td class="s">%s</td><td class="t">%s</td></tr>`,
			icon, href, html.EscapeString(n), size, e.ModTime().Format("2006-01-02 15:04"))
	}
	fmt.Fprint(w, `</table></body></html>`)
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
