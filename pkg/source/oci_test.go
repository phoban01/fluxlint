package source

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/phoban01/fluxlint/pkg/model"
)

const fluxLayer = types.MediaType("application/vnd.cncf.flux.content.v1.tar+gzip")

func tarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func push(t *testing.T, ref string, files map[string]string) {
	t.Helper()
	img, err := mutate.AppendLayers(empty.Image, static.NewLayer(tarGz(t, files), fluxLayer))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := name.ParseReference(ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(parsed, img); err != nil {
		t.Fatal(err)
	}
}

func ociSource(url string, ref map[string]any) model.Object {
	return model.Object{
		"apiVersion": "source.toolkit.fluxcd.io/v1",
		"kind":       "OCIRepository",
		"metadata":   map[string]any{"name": "manifests", "namespace": "flux-system"},
		"spec":       map[string]any{"url": url, "ref": ref, "insecure": true},
	}
}

func TestOCIRepository(t *testing.T) {
	srv := httptest.NewServer(registry.New())
	host := strings.TrimPrefix(srv.URL, "http://")
	repo := host + "/team/manifests"
	push(t, repo+":1.0.0", map[string]string{"deploy/cm.yaml": "one"})
	push(t, repo+":1.2.0", map[string]string{"deploy/cm.yaml": "two"})

	cache := t.TempDir()
	r, err := New(Options{CacheDir: cache})
	if err != nil {
		t.Fatal(err)
	}
	read := func(res *Result) string {
		b, err := os.ReadFile(filepath.Join(res.Dir, "deploy/cm.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	pinned, err := r.Resolve(context.Background(), ociSource("oci://"+repo, map[string]any{"tag": "1.0.0"}))
	if err != nil {
		t.Fatal(err)
	}
	if read(pinned) != "one" || pinned.Floating != "" || !strings.Contains(pinned.Revision, "@sha256:") {
		t.Errorf("tag 1.0.0: content %q, %+v", read(pinned), pinned)
	}

	ranged, err := r.Resolve(context.Background(), ociSource("oci://"+repo, map[string]any{"semver": ">=1.0.0"}))
	if err != nil {
		t.Fatal(err)
	}
	if read(ranged) != "two" || ranged.Floating == "" {
		t.Errorf("semver must pick 1.2.0 and be flagged floating: content %q, %+v", read(ranged), ranged)
	}

	// registry gone: the pinned artifact is served from cache, offline
	srv.Close()
	offline, _ := New(Options{CacheDir: cache, Mode: Offline})
	again, err := offline.Resolve(context.Background(), ociSource("oci://"+repo, map[string]any{"tag": "1.0.0"}))
	if err != nil || read(again) != "one" {
		t.Errorf("warm cache must serve offline: %v", err)
	}
	if _, err := offline.Resolve(context.Background(), ociSource("oci://"+repo, map[string]any{"tag": "9.9.9"})); !errors.Is(err, ErrNotCached) {
		t.Errorf("offline miss: %v", err)
	}
}

func TestUntarRefusesPathTraversal(t *testing.T) {
	evil := tarGz(t, map[string]string{"../../escaped.yaml": "x"})
	dst := t.TempDir()
	if err := untarGz(bytes.NewReader(evil), dst); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("path traversal not refused: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(filepath.Dir(dst)), "escaped.yaml")); err == nil {
		t.Fatal("file was written outside the extraction directory")
	}
}
