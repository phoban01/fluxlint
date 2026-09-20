// Package source materialises Flux sources other than the repository itself
// into local directories, through a content-addressed cache.
package source

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/phoban01/fluxlint/pkg/model"
)

// Mode says how much network the resolver may use.
type Mode int

const (
	// FetchMissing uses the cache and fetches what is not in it (default).
	FetchMissing Mode = iota
	// Offline never touches the network.
	Offline
	// Refresh additionally re-resolves floating refs (branches, semver ranges).
	Refresh
)

// Override maps a source object to a local directory, e.g. a sibling checkout.
type Override struct {
	Kind, Namespace, Name, Path string
}

// Options configures a Resolver.
type Options struct {
	CacheDir  string
	Mode      Mode
	Overrides []Override
	Timeout   time.Duration // per fetch; default 60s
}

// Result is a source made available on disk.
type Result struct {
	Dir      string // root of the artifact
	Revision string // e.g. "v1.2.3@sha1:abc…"
	// Floating is set when the ref can move (branch, semver range): results
	// are then not reproducible from the Git tree alone.
	Floating string
}

// ErrUnsupported marks source kinds fluxlint cannot fetch yet.
var ErrUnsupported = errors.New("source kind not supported yet")

// ErrNotCached is returned in Offline mode on a cache miss.
var ErrNotCached = errors.New("not in cache and running offline")

// Resolver fetches sources. It is safe for concurrent use; each distinct
// source is resolved once per Resolver.
type Resolver struct {
	opts Options
	mu   sync.Mutex
	once map[string]*call
	// down records hosts that could not be reached, so that one dead registry
	// costs a single connect timeout rather than one per artifact.
	down map[string]error
	// indexes memoises Helm repository index downloads for the run.
	indexes map[string]*indexCall
}

// connectTimeout bounds how long an unreachable host can hold up a run.
const connectTimeout = 5 * time.Second

func (r *Resolver) hostDown(host string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.down[host]
}

func (r *Resolver) markDown(host string, err error) {
	var op *net.OpError
	if !errors.As(err, &op) || op.Op != "dial" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.down[host] = fmt.Errorf("%s is unreachable: %w", host, op.Err)
}

type call struct {
	done chan struct{}
	res  *Result
	err  error
}

// New returns a Resolver. An empty CacheDir selects the user cache directory.
func New(opts Options) (*Resolver, error) {
	if opts.CacheDir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			return nil, err
		}
		opts.CacheDir = filepath.Join(base, "fluxlint")
	}
	if opts.Timeout == 0 {
		opts.Timeout = 60 * time.Second
	}
	return &Resolver{opts: opts, once: map[string]*call{}, down: map[string]error{}, indexes: map[string]*indexCall{}}, nil
}

// Resolve makes the artifact of a Flux source object available locally.
func (r *Resolver) Resolve(ctx context.Context, src model.Object) (*Result, error) {
	// Deduplicate by what is fetched, not by object identity: two entrypoints
	// pinning the same repository and ref share one cache slot.
	key := src.ID() + "|" + specHash(src)
	switch src.Kind() {
	case "GitRepository":
		key = "git|" + model.Str(src, "spec", "url") + "|" + parseGitRef(src).display
	case "OCIRepository":
		ref, _ := json.Marshal(model.Get(src, "spec", "ref"))
		key = "oci|" + model.Str(src, "spec", "url") + "|" + string(ref) + "|" + model.Str(src, "spec", "layerSelector", "mediaType")
	}
	for _, o := range r.opts.Overrides {
		if o.Kind == src.Kind() && o.Name == src.Name() {
			key = src.ID() // overrides are per object
		}
	}
	r.mu.Lock()
	c, inflight := r.once[key]
	if !inflight {
		c = &call{done: make(chan struct{})}
		r.once[key] = c
	}
	r.mu.Unlock()
	if inflight {
		<-c.done
		return c.res, c.err
	}
	c.res, c.err = r.resolve(ctx, src)
	close(c.done)
	return c.res, c.err
}

func (r *Resolver) resolve(ctx context.Context, src model.Object) (*Result, error) {
	for _, o := range r.opts.Overrides {
		if o.Kind == src.Kind() && o.Name == src.Name() && (o.Namespace == "" || o.Namespace == src.Namespace()) {
			dir, err := filepath.Abs(o.Path)
			if err != nil {
				return nil, err
			}
			if st, err := os.Stat(dir); err != nil || !st.IsDir() {
				return nil, fmt.Errorf("override path %q is not a directory", o.Path)
			}
			return &Result{Dir: dir, Revision: "override:" + o.Path}, nil
		}
	}
	ctx, cancel := context.WithTimeout(ctx, r.opts.Timeout)
	defer cancel()
	switch src.Kind() {
	case "GitRepository":
		return r.git(ctx, src)
	case "OCIRepository":
		return r.oci(ctx, src)
	default:
		return nil, fmt.Errorf("%s: %w", src.Kind(), ErrUnsupported)
	}
}

// specHash distinguishes two objects with the same identity but different
// specs (the same source name rendered for two entrypoints).
func specHash(src model.Object) string {
	b, _ := json.Marshal(model.Get(src, "spec"))
	return hashOf(string(b))
}

func hashOf(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// meta is stored next to each cached tree.
type meta struct {
	URL      string    `json:"url"`
	Ref      string    `json:"ref"`
	Revision string    `json:"revision"`
	Fetched  time.Time `json:"fetched"`
}

func readMeta(path string) (*meta, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	m := &meta{}
	return m, json.Unmarshal(b, m) == nil
}
