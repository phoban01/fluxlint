package source

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
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
	c.once.Do(func() { c.data, c.err = r.kubeFile(ctx, base, release, "api/openapi-spec/v3", file) })
	return c.data, c.err
}

// kubeFile reads one file that a Kubernetes release publishes in its
// repository: an OpenAPI document, or a discovery document.
func (r *Resolver) kubeFile(ctx context.Context, base, release, dir, file string) ([]byte, error) {
	rel := filepath.Join(release, filepath.FromSlash(dir), file)
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
		if file != "api__v1_openapi.json" && dir == "api/openapi-spec/v3" {
			if _, err := r.kubeFile(ctx, base, release, "api/openapi-spec/v3", "api__v1_openapi.json"); err != nil {
				return nil, fmt.Errorf("the API schemas of Kubernetes %s are not at %s: is kubeVersion a released version?", release, base)
			}
		} else if dir == "api/openapi-spec/v3" {
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

// KubeAPIVersions returns what a cluster of the given release answers when
// asked which APIs it serves: every "group/version" and
// "group/version/Kind", as Helm's .Capabilities.APIVersions holds them. It is
// read from the discovery documents the release publishes. Alpha versions are
// left out: a cluster does not serve them unless told to.
func (r *Resolver) KubeAPIVersions(ctx context.Context, base, release string) ([]string, error) {
	if base == "" {
		base = DefaultSchemaBase
	}
	set := map[string]bool{}
	add := func(group, version, kind string) {
		if strings.Contains(version, "alpha") {
			return
		}
		gv := version
		if group != "" {
			gv = group + "/" + version
		}
		set[gv] = true
		if kind != "" {
			set[gv+"/"+kind] = true
		}
	}
	core, err := r.kubeFile(ctx, base, release, "api/discovery", "api__v1.json")
	if err != nil {
		return nil, err
	}
	var list struct {
		Resources []struct{ Name, Kind string }
	}
	if err := json.Unmarshal(core, &list); err != nil {
		return nil, err
	}
	for _, res := range list.Resources {
		if !strings.Contains(res.Name, "/") {
			add("", "v1", res.Kind)
		}
	}
	var groups []byte
	for _, file := range []string{"aggregated_v2.json", "aggregated_v2beta1.json"} {
		if groups, err = r.kubeFile(ctx, base, release, "api/discovery", file); err == nil {
			break
		}
	}
	if err != nil {
		return nil, err
	}
	var agg struct {
		Items []struct {
			Metadata struct{ Name string }
			Versions []struct {
				Version   string
				Resources []struct {
					Resource     string
					ResponseKind struct{ Kind string }
				}
			}
		}
	}
	if err := json.Unmarshal(groups, &agg); err != nil {
		return nil, err
	}
	for _, g := range agg.Items {
		for _, v := range g.Versions {
			add(g.Metadata.Name, v.Version, "")
			for _, res := range v.Resources {
				add(g.Metadata.Name, v.Version, res.ResponseKind.Kind)
			}
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}
