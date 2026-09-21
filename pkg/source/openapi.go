package source

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// DefaultSchemaBase is where Kubernetes publishes the OpenAPI documents of
// every release, next to the code they are generated from.
const DefaultSchemaBase = "https://raw.githubusercontent.com/kubernetes/kubernetes"

type schemaCall struct {
	once sync.Once
	data []byte
	err  error
}

// KubeRelease turns "1.35", "v1.35.2" or "1.35.0" into the tag "v1.35.2"
// (patch 0 when none is given: the API surface of a minor does not change).
func KubeRelease(kubeVersion string) (string, error) {
	v := strings.TrimPrefix(strings.TrimSpace(kubeVersion), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) == 2 {
		parts = append(parts, "0")
	}
	if len(parts) != 3 {
		return "", fmt.Errorf("kubeVersion %q is not major.minor[.patch]", kubeVersion)
	}
	for _, p := range parts {
		if p == "" || strings.Trim(p, "0123456789") != "" {
			return "", fmt.Errorf("kubeVersion %q is not major.minor[.patch]", kubeVersion)
		}
	}
	return "v" + strings.Join(parts, "."), nil
}

// KubeSchema returns the OpenAPI v3 document of one group and version as the
// given Kubernetes release publishes it. base is a URL prefix or a local
// directory, laid out like the Kubernetes repository:
// <base>/<tag>/api/openapi-spec/v3/<file>. A release that does not serve the
// group and version gives ErrNotFound. Documents are immutable per release,
// so a cached one is never fetched again.
func (r *Resolver) KubeSchema(ctx context.Context, base, release, group, version string) ([]byte, error) {
	if base == "" {
		base = DefaultSchemaBase
	}
	file := "api__" + version + "_openapi.json"
	if group != "" {
		file = "apis__" + group + "__" + version + "_openapi.json"
	}
	key := base + "|" + release + "|" + file
	r.mu.Lock()
	if r.schemas == nil {
		r.schemas = map[string]*schemaCall{}
	}
	c := r.schemas[key]
	if c == nil {
		c = &schemaCall{}
		r.schemas[key] = c
	}
	r.mu.Unlock()
	c.once.Do(func() { c.data, c.err = r.kubeSchema(ctx, base, release, file) })
	return c.data, c.err
}

func (r *Resolver) kubeSchema(ctx context.Context, base, release, file string) ([]byte, error) {
	rel := filepath.Join(release, "api", "openapi-spec", "v3", file)
	if !strings.Contains(base, "://") {
		b, err := os.ReadFile(filepath.Join(base, rel))
		if os.IsNotExist(err) {
			if _, statErr := os.Stat(filepath.Join(base, release)); statErr == nil {
				return nil, fmt.Errorf("%s in Kubernetes %s: %w", file, release, ErrNotFound)
			}
		}
		return b, err
	}
	slot := filepath.Join(r.opts.CacheDir, "openapi", hashOf(base), rel)
	if b, err := os.ReadFile(slot); err == nil {
		return b, nil
	}
	if _, err := os.Stat(slot + ".absent"); err == nil {
		return nil, fmt.Errorf("%s in Kubernetes %s: %w", file, release, ErrNotFound)
	}
	if r.opts.Mode == Offline {
		return nil, fmt.Errorf("the API schemas of Kubernetes %s: %w", release, ErrNotCached)
	}
	u, err := url.Parse(base)
	if err != nil {
		return nil, err
	}
	if err := r.hostDown(u.Host); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, r.opts.Timeout)
	defer cancel()
	target := strings.TrimSuffix(base, "/") + "/" + filepath.ToSlash(rel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.markDown(u.Host, err)
		return nil, err
	}
	defer resp.Body.Close()
	if err := os.MkdirAll(filepath.Dir(slot), 0o755); err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		// either the release does not serve this group and version, or there
		// is no such release: the core document tells the two apart
		if file != "api__v1_openapi.json" {
			if _, err := r.kubeSchema(ctx, base, release, "api__v1_openapi.json"); err != nil {
				return nil, fmt.Errorf("the API schemas of Kubernetes %s are not at %s: is kubeVersion a released version?", release, base)
			}
		} else {
			return nil, fmt.Errorf("the API schemas of Kubernetes %s are not at %s: is kubeVersion a released version?", release, base)
		}
		_ = os.WriteFile(slot+".absent", nil, 0o644)
		return nil, fmt.Errorf("%s in Kubernetes %s: %w", file, release, ErrNotFound)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("GET %s: %s", target, resp.Status)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	tmp := slot + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return nil, err
	}
	return b, os.Rename(tmp, slot)
}
