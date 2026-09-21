package source

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/phoban01/fluxlint/pkg/model"
	"helm.sh/helm/v3/pkg/repo"
)

// Chart makes the chart of a HelmRelease available locally. src is the object
// its chart.spec.sourceRef / chartRef points at. The result's Dir is a chart
// directory or a packaged .tgz, either of which the Helm loader accepts.
func (r *Resolver) Chart(ctx context.Context, hr, src model.Object) (*Result, error) {
	ctx, cancel := context.WithTimeout(ctx, r.opts.Timeout)
	defer cancel()

	name := model.Str(hr, "spec", "chart", "spec", "chart")
	version := model.Str(hr, "spec", "chart", "spec", "version")

	switch src.Kind() {
	case "HelmRepository":
		url := model.Str(src, "spec", "url")
		if model.Str(src, "spec", "type") == "oci" || strings.HasPrefix(url, "oci://") {
			// An OCI Helm chart is an artifact whose first layer is the chart
			// tarball, so the OCIRepository resolver does the work.
			ref := map[string]any{"semver": version}
			if version == "" {
				ref = map[string]any{"semver": "*"}
			} else if _, err := semver.StrictNewVersion(strings.TrimPrefix(version, "v")); err == nil {
				ref = map[string]any{"tag": version}
			}
			res, err := r.Resolve(ctx, model.Object{
				"kind":     "OCIRepository",
				"metadata": map[string]any{"name": src.Name() + "/" + name, "namespace": src.Namespace()},
				"spec": map[string]any{
					"url": strings.TrimSuffix(url, "/") + "/" + name, "ref": ref,
					"insecure": model.Bool(src, "spec", "insecure"),
				},
			})
			if err != nil {
				return nil, err
			}
			return chartRoot(res)
		}
		return r.httpChart(ctx, url, name, version)

	case "OCIRepository":
		res, err := r.Resolve(ctx, src)
		if err != nil {
			return nil, err
		}
		return chartRoot(res)

	case "GitRepository":
		res, err := r.Resolve(ctx, src)
		if err != nil {
			return nil, err
		}
		out := *res
		out.Dir = filepath.Join(res.Dir, name)
		if _, err := os.Stat(filepath.Join(out.Dir, "Chart.yaml")); err != nil {
			return nil, fmt.Errorf("no Chart.yaml at %q in %s", name, src)
		}
		return &out, nil

	default:
		return nil, fmt.Errorf("chart source %s: %w", src.Kind(), ErrUnsupported)
	}
}

// chartRoot finds the chart inside an extracted artifact: either the root or
// its only sub-directory (Helm packages charts as <name>/…).
func chartRoot(res *Result) (*Result, error) {
	out := *res
	if _, err := os.Stat(filepath.Join(res.Dir, "Chart.yaml")); err == nil {
		return &out, nil
	}
	entries, err := os.ReadDir(res.Dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() {
			if _, err := os.Stat(filepath.Join(res.Dir, e.Name(), "Chart.yaml")); err == nil {
				out.Dir = filepath.Join(res.Dir, e.Name())
				return &out, nil
			}
		}
	}
	return nil, fmt.Errorf("artifact %s contains no Chart.yaml", res.Revision)
}

// httpChart resolves name@version against a classic HTTP(S) Helm repository.
func (r *Resolver) httpChart(ctx context.Context, repoURL, name, version string) (*Result, error) {
	display := name + " " + version
	floating := ""
	if _, err := semver.StrictNewVersion(strings.TrimPrefix(version, "v")); err != nil {
		if version == "" {
			display = name + " (latest)"
		}
		floating = "chart version " + fmt.Sprintf("%q", version) + " is a range: it resolves to whatever the repository serves"
	}
	slot := filepath.Join(r.opts.CacheDir, "helm", hashOf(repoURL, name, version)) + ".tgz"
	metaPath := slot + ".json"
	if m, ok := readMeta(metaPath); ok && fileExists(slot) {
		if floating == "" || r.opts.Mode != Refresh {
			return &Result{Dir: slot, Revision: m.Revision, Floating: floating}, nil
		}
	}
	if r.opts.Mode == Offline {
		return nil, fmt.Errorf("%s (%s): %w", repoURL, display, ErrNotCached)
	}
	u, err := neturl.Parse(repoURL)
	if err != nil {
		return nil, err
	}
	if err := r.hostDown(u.Host); err != nil {
		return nil, fmt.Errorf("%s (%s): %w", repoURL, display, err)
	}

	index, err := r.helmIndex(ctx, repoURL)
	if err != nil {
		r.markDown(u.Host, err)
		return nil, fmt.Errorf("%s: %w", repoURL, err)
	}
	cv, err := index.Get(name, version)
	if err != nil {
		return nil, fmt.Errorf("%s: chart %s: %v: %w", repoURL, display, err, ErrNotFound)
	}
	if len(cv.URLs) == 0 {
		return nil, fmt.Errorf("%s: chart %s %s has no download URL", repoURL, name, cv.Version)
	}
	chartURL, err := repo.ResolveReferenceURL(repoURL, cv.URLs[0])
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(slot), 0o755); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(slot), "fetch-")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	if err := httpGet(ctx, chartURL, tmp); err != nil {
		tmp.Close()
		return nil, fmt.Errorf("%s: %w", chartURL, err)
	}
	tmp.Close()
	if err := os.Rename(tmp.Name(), slot); err != nil {
		return nil, err
	}
	m := meta{URL: repoURL, Ref: display, Revision: name + " " + cv.Version, Fetched: time.Now().UTC()}
	b, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(metaPath, b, 0o644); err != nil {
		return nil, err
	}
	return &Result{Dir: slot, Revision: m.Revision, Floating: floating}, nil
}

// helmIndex downloads and parses index.yaml once per repository per run.
func (r *Resolver) helmIndex(ctx context.Context, repoURL string) (*repo.IndexFile, error) {
	r.mu.Lock()
	c, inflight := r.indexes[repoURL]
	if !inflight {
		c = &indexCall{done: make(chan struct{})}
		r.indexes[repoURL] = c
	}
	r.mu.Unlock()
	if inflight {
		<-c.done
		return c.index, c.err
	}
	defer close(c.done)

	tmp, err := os.CreateTemp("", "fluxlint-index-")
	if err != nil {
		c.err = err
		return nil, err
	}
	defer os.Remove(tmp.Name())
	c.err = httpGet(ctx, strings.TrimSuffix(repoURL, "/")+"/index.yaml", tmp)
	tmp.Close()
	if c.err == nil {
		c.index, c.err = repo.LoadIndexFile(tmp.Name())
	}
	return c.index, c.err
}

type indexCall struct {
	done  chan struct{}
	index *repo.IndexFile
	err   error
}

var httpClient = &http.Client{Transport: func() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = (&net.Dialer{Timeout: connectTimeout}).DialContext
	t.TLSHandshakeTimeout = connectTimeout
	return t
}()}

func httpGet(ctx context.Context, url string, w io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if login, password, ok := netrcCredentials(req.URL.Host); ok {
		req.SetBasicAuth(login, password)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("GET %s: %s (credentials for HTTP Helm repositories are read from $NETRC or ~/.netrc)", url, resp.Status)
	default:
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	_, err = io.Copy(w, io.LimitReader(resp.Body, maxArtifactBytes))
	return err
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}
