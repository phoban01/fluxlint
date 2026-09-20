package source

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/phoban01/fluxlint/pkg/model"
)

// oci materialises an OCIRepository artifact: the (selected) layer is a
// gzipped tarball of manifests, as produced by `flux push artifact`.
func (r *Resolver) oci(ctx context.Context, src model.Object) (*Result, error) {
	url := model.Str(src, "spec", "url")
	repo, ok := strings.CutPrefix(url, "oci://")
	if !ok {
		return nil, fmt.Errorf("OCIRepository %s: spec.url must start with oci://", src)
	}
	ref := func(k string) string { return model.Str(src, "spec", "ref", k) }
	var nameOpts []name.Option
	if model.Bool(src, "spec", "insecure") {
		nameOpts = append(nameOpts, name.Insecure)
	}
	transport := remote.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{Timeout: connectTimeout}).DialContext
	transport.TLSHandshakeTimeout = connectTimeout
	remoteOpts := []remote.Option{remote.WithContext(ctx), remote.WithAuthFromKeychain(authn.DefaultKeychain), remote.WithTransport(transport)}
	host, _, _ := strings.Cut(repo, "/")

	// source-controller's precedence: digest > semver > tag (default "latest")
	var target, display, floating string
	switch {
	case ref("digest") != "":
		target, display = repo+"@"+ref("digest"), "digest "+ref("digest")
	case ref("semver") != "":
		display = "semver " + ref("semver")
		floating = "semver range " + ref("semver") + " resolves to whatever tag is newest"
	default:
		tag := ref("tag")
		if tag == "" {
			tag = "latest"
		}
		target, display = repo+":"+tag, "tag "+tag
		if tag == "latest" {
			floating = "tag latest moves with every push"
		}
	}

	slot := filepath.Join(r.opts.CacheDir, "oci", hashOf(url, display, model.Str(src, "spec", "layerSelector", "mediaType")))
	metaPath := slot + ".json"
	if m, ok := readMeta(metaPath); ok && dirExists(slot) {
		if floating == "" || r.opts.Mode != Refresh {
			return &Result{Dir: slot, Revision: m.Revision, Floating: floating}, nil
		}
	}
	if r.opts.Mode == Offline {
		return nil, fmt.Errorf("%s (%s): %w", url, display, ErrNotCached)
	}
	if err := r.hostDown(host); err != nil {
		return nil, fmt.Errorf("%s (%s): %w", url, display, err)
	}

	if target == "" { // semver
		rp, err := name.NewRepository(repo, nameOpts...)
		if err != nil {
			return nil, err
		}
		tags, err := remote.List(rp, remoteOpts...)
		if err != nil {
			r.markDown(host, err)
			return nil, fmt.Errorf("%s: %w", url, err)
		}
		tag, err := newestMatching(tags, ref("semver"))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", url, err)
		}
		target = repo + ":" + tag
	}
	parsed, err := name.ParseReference(target, nameOpts...)
	if err != nil {
		return nil, err
	}
	img, err := remote.Image(parsed, remoteOpts...)
	if err != nil {
		r.markDown(host, err)
		return nil, fmt.Errorf("%s (%s): %w", url, display, err)
	}
	digest, err := img.Digest()
	if err != nil {
		return nil, err
	}
	layers, err := img.Layers()
	if err != nil {
		return nil, err
	}
	if len(layers) == 0 {
		return nil, fmt.Errorf("%s (%s): artifact has no layers", url, display)
	}
	layer := layers[0]
	if want := model.Str(src, "spec", "layerSelector", "mediaType"); want != "" {
		layer = nil
		for _, l := range layers {
			if mt, _ := l.MediaType(); string(mt) == want {
				layer = l
				break
			}
		}
		if layer == nil {
			return nil, fmt.Errorf("%s (%s): no layer with media type %s", url, display, want)
		}
	}

	if err := os.MkdirAll(filepath.Dir(slot), 0o755); err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(slot), "fetch-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	blob, err := layer.Compressed()
	if err != nil {
		return nil, err
	}
	defer blob.Close()
	if err := untarGz(blob, tmp); err != nil {
		return nil, fmt.Errorf("%s (%s): %w", url, display, err)
	}

	m := meta{URL: url, Ref: display, Revision: display + "@" + digest.String(), Fetched: time.Now().UTC()}
	_ = os.RemoveAll(slot)
	if err := os.Rename(tmp, slot); err != nil && !dirExists(slot) {
		return nil, err
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(metaPath, b, 0o644); err != nil {
		return nil, err
	}
	return &Result{Dir: slot, Revision: m.Revision, Floating: floating}, nil
}

func newestMatching(tags []string, constraint string) (string, error) {
	c, err := semver.NewConstraint(constraint)
	if err != nil {
		return "", fmt.Errorf("invalid semver range %q: %w", constraint, err)
	}
	type candidate struct {
		v   *semver.Version
		tag string
	}
	var matches []candidate
	for _, t := range tags {
		if v, err := semver.NewVersion(t); err == nil && c.Check(v) {
			matches = append(matches, candidate{v, t})
		}
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("no tag matches semver range %q", constraint)
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].v.LessThan(matches[j].v) })
	return matches[len(matches)-1].tag, nil
}

const maxArtifactBytes = 512 << 20

// untarGz extracts regular files and directories only, refusing any entry
// that would land outside dst.
func untarGz(r io.Reader, dst string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("artifact layer is not gzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var written int64
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		target := filepath.Join(dst, filepath.FromSlash(h.Name))
		if rel, err := filepath.Rel(dst, target); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("artifact entry %q escapes the extraction directory", h.Name)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			if err != nil {
				return err
			}
			n, err := io.Copy(f, io.LimitReader(tr, maxArtifactBytes-written+1))
			f.Close()
			if err != nil {
				return err
			}
			if written += n; written > maxArtifactBytes {
				return fmt.Errorf("artifact exceeds %d bytes", maxArtifactBytes)
			}
		default:
			// symlinks, devices, …: manifests never need them
		}
	}
}
