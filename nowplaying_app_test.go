package main

import (
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
)

// The page for OBS is copied next to its font; unchanged files are not
// rewritten (so OBS only reloads the page after an update changed it).
func TestWriteOverlayFiles(t *testing.T) {
	dir := t.TempDir()
	files := fstest.MapFS{
		assetsPrefix + overlayPage: {Data: []byte("<html>v1</html>")},
		assetsPrefix + overlayFont: {Data: []byte("font")},
	}
	page, changed, err := writeOverlayFiles(dir, files)
	if err != nil || !changed || page != filepath.Join(dir, "overlay", "nowplaying.html") {
		t.Fatalf("first: %q %v %v", page, changed, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "fonts", "archivo-latin-wdth-normal.woff2")); string(b) != "font" {
		t.Fatal("the font must sit where the page loads it (../fonts/)")
	}
	if _, changed, err = writeOverlayFiles(dir, files); err != nil || changed {
		t.Fatalf("unchanged: %v %v", changed, err)
	}
	files[assetsPrefix+overlayPage] = &fstest.MapFile{Data: []byte("<html>v2</html>")}
	if _, changed, _ = writeOverlayFiles(dir, files); !changed {
		t.Fatal("a new page must be written")
	}
	if b, _ := os.ReadFile(page); string(b) != "<html>v2</html>" {
		t.Fatalf("page %q", b)
	}
}

// The real embedded files exist (a renamed file would break OBS silently).
func TestEmbeddedOverlayFiles(t *testing.T) {
	if _, _, err := writeOverlayFiles(t.TempDir(), assets); err != nil {
		t.Fatal(err)
	}
}

func TestFetchCoverOnlyHTTPS(t *testing.T) {
	for _, u := range []string{"http://example.com/a.jpg", "file:///etc/passwd", "javascript:alert(1)", ""} {
		if _, _, err := fetchCover(t.Context(), u); err == nil {
			t.Errorf("%q must be refused", u)
		}
	}
}
