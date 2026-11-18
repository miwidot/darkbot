package secret

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/zalando/go-keyring"
)

type nopLog struct{}

func (nopLog) Warnf(string, ...any) {}

type value struct {
	AccessToken string `json:"access_token"`
}

func TestCorruptFileIsIgnored(t *testing.T) {
	p := filepath.Join(t.TempDir(), "token.json")
	if err := os.WriteFile(p, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &Store{Key: "test", FallbackPath: p, Log: nopLog{}}
	var v value
	if ok, err := s.loadFile(&v); ok || err != nil {
		t.Fatalf("corrupt file must read as nothing stored, got %v, %v", ok, err)
	}
	if err := WriteFileAtomic(p, []byte(`{"access_token":"A"}`)); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.loadFile(&v); !ok || v.AccessToken != "A" {
		t.Fatal("atomic write not readable")
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != filePerm {
			t.Fatalf("file mode %v, want %v", fi.Mode().Perm(), os.FileMode(filePerm))
		}
	}
}

func TestMissingFileIsNothingStored(t *testing.T) {
	s := &Store{Key: "test", FallbackPath: filepath.Join(t.TempDir(), "none.json"), Log: nopLog{}}
	var v value
	if ok, err := s.loadFile(&v); ok || err != nil {
		t.Fatalf("got %v, %v", ok, err)
	}
}

// The keyring failed at the last save, so the newest token is in the file;
// the older one still in the keyring (a used, dead refresh token) must not
// win. Once the keyring works again, saving moves it back.
func TestNewerFallbackFileWins(t *testing.T) {
	keyring.MockInit()
	t.Cleanup(keyring.MockInit)
	p := filepath.Join(t.TempDir(), "token.json")
	s := &Store{Key: "test", FallbackPath: p, Log: nopLog{}}
	if err := s.Save(value{AccessToken: "old"}); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(p, []byte(`{"access_token":"new"}`)); err != nil { // keyring save failed
		t.Fatal(err)
	}
	var v value
	if ok, err := s.Load(&v); !ok || err != nil || v.AccessToken != "new" {
		t.Fatalf("loaded %q (%v %v), want the newer file", v.AccessToken, ok, err)
	}
	if err := s.Save(value{AccessToken: "newest"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("a working keyring save must remove the file")
	}
	if ok, _ := s.Load(&v); !ok || v.AccessToken != "newest" {
		t.Fatalf("loaded %q", v.AccessToken)
	}
}
