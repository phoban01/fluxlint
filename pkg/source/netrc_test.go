package source

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNetrc(t *testing.T) {
	path := filepath.Join(t.TempDir(), "netrc")
	os.WriteFile(path, []byte("machine gitlab.example.test login ci password s3cret\nmachine other.test\n  login o\n  password p\ndefault login anon password none\n"), 0o600)
	t.Setenv("NETRC", path)
	for host, want := range map[string][2]string{
		"gitlab.example.test":     {"ci", "s3cret"},
		"gitlab.example.test:443": {"ci", "s3cret"},
		"other.test":              {"o", "p"},
		"unknown.test":            {"anon", "none"},
	} {
		l, p, ok := netrcCredentials(host)
		if !ok || l != want[0] || p != want[1] {
			t.Errorf("%s: got %q %q %v", host, l, p, ok)
		}
	}
	t.Setenv("NETRC", filepath.Join(t.TempDir(), "missing"))
	if _, _, ok := netrcCredentials("gitlab.example.test"); ok {
		t.Error("no file, no credentials")
	}
}
