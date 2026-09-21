package source

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
)

// ImageStatus is what a registry says about one image reference.
type ImageStatus struct {
	Image  string `json:"image"`
	Exists bool   `json:"exists"`
	// Platforms are "os/arch" pairs the reference can be pulled for. Empty
	// when the registry did not say (a single-platform image is not opened).
	Platforms []string  `json:"platforms,omitempty"`
	Checked   time.Time `json:"checked"`
}

type imageCall struct {
	once   sync.Once
	status ImageStatus
	err    error
}

// Image asks the registry whether an image reference can be pulled. "It is
// not there" is an answer, not an error; an error means the question could
// not be settled (no credentials, registry unreachable, offline and not
// cached). An image found once is cached: a tag that exists rarely stops
// existing. One that was missing is asked about again on the next run.
func (r *Resolver) Image(ctx context.Context, image string) (ImageStatus, error) {
	r.mu.Lock()
	if r.images == nil {
		r.images = map[string]*imageCall{}
	}
	c := r.images[image]
	if c == nil {
		c = &imageCall{}
		r.images[image] = c
	}
	r.mu.Unlock()
	c.once.Do(func() { c.status, c.err = r.image(ctx, image) })
	return c.status, c.err
}

func (r *Resolver) image(ctx context.Context, image string) (ImageStatus, error) {
	ref, err := name.ParseReference(image)
	if err != nil {
		return ImageStatus{}, err
	}
	slot := filepath.Join(r.opts.CacheDir, "images", hashOf(ref.Name())+".json")
	_, pinned := ref.(name.Digest)
	if b, err := os.ReadFile(slot); err == nil && (pinned || r.opts.Mode != Refresh) {
		var st ImageStatus
		if json.Unmarshal(b, &st) == nil && st.Exists {
			return st, nil
		}
	}
	if r.opts.Mode == Offline {
		return ImageStatus{}, fmt.Errorf("%s: %w", image, ErrNotCached)
	}
	host := ref.Context().RegistryStr()
	if err := r.hostDown(host); err != nil {
		return ImageStatus{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, r.opts.Timeout)
	defer cancel()
	opts := remoteOptions(ctx)

	st := ImageStatus{Image: ref.Name(), Checked: time.Now().UTC()}
	desc, err := remote.Head(ref, opts...)
	if err != nil {
		var te *transport.Error
		if errors.As(err, &te) && te.StatusCode == http.StatusNotFound {
			return st, nil
		}
		r.markDown(host, err)
		return ImageStatus{}, err
	}
	st.Exists = true
	if desc.MediaType.IsIndex() {
		if idx, err := remote.Index(ref, opts...); err == nil {
			if m, err := idx.IndexManifest(); err == nil {
				seen := map[string]bool{}
				for _, d := range m.Manifests {
					if p := d.Platform; p != nil && p.OS != "" && p.OS != "unknown" && !seen[p.OS+"/"+p.Architecture] {
						seen[p.OS+"/"+p.Architecture] = true
						st.Platforms = append(st.Platforms, p.OS+"/"+p.Architecture)
					}
				}
				sort.Strings(st.Platforms)
			}
		}
	}
	if err := os.MkdirAll(filepath.Dir(slot), 0o755); err != nil {
		return st, nil
	}
	b, _ := json.MarshalIndent(st, "", "  ")
	_ = os.WriteFile(slot, b, 0o644)
	return st, nil
}
