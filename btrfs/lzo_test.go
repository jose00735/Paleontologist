package btrfs

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLZOVectors(t *testing.T) {
	dir := os.Getenv("LZO_VECTORS")
	if dir == "" {
		t.Skip("LZO_VECTORS not set")
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.bin"))
	for _, f := range files {
		want, _ := os.ReadFile(f)
		for _, ext := range []string{".lz1", ".lz9"} {
			c, err := os.ReadFile(strings.TrimSuffix(f, ".bin") + ext)
			if err != nil {
				t.Fatal(err)
			}
			got, err := lzo1xDecompress(c, len(want))
			if err != nil {
				t.Fatalf("%s%s: %v", f, ext, err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("%s%s: mismatch (got %d bytes, want %d)", f, ext, len(got), len(want))
			}
		}
	}
	t.Logf("%d vectors ok", len(files)*2)
}
