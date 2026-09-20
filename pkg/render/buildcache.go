package render

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"path/filepath"
	"strings"
	"sync"

	"github.com/phoban01/fluxlint/pkg/model"
	"sigs.k8s.io/kustomize/kyaml/filesys"
)

// BuildCache lets renders of related trees share kustomize builds. `check
// --base` renders two commits that differ in a handful of files; a build is
// reused when everything kustomize looked at the first time — every file it
// read, every directory it listed, every path it probed and found missing —
// is unchanged in the tree being rendered now. Entrypoints that build the same
// directory with the same overlay share builds too.
type BuildCache struct {
	mu      sync.Mutex
	entries map[string][]*cachedBuild
	charts  map[string]*cachedChart
	hits    int
	misses  int
}

func NewBuildCache() *BuildCache {
	return &BuildCache{entries: map[string][]*cachedBuild{}, charts: map[string]*cachedChart{}}
}

// Stats reports how many builds were reused and how many ran.
func (c *BuildCache) Stats() (hits, misses int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits, c.misses
}

// probe is one observation of the source tree, relative to its root.
type probe struct {
	op  byte // 'f' file content, 'd' directory listing, 's' path state
	rel string
	sum string
}

type cachedBuild struct {
	probes  []probe
	objs    []byte   // JSON, so every user gets its own copy
	origins []string // relative to the root; "" when unknown
}

func overlayKey(relDir string, ov overlay) string {
	b, _ := json.Marshal([]any{relDir, ov.Patches, ov.Images, ov.Components, ov.TargetNamespace, ov.NamePrefix, ov.NameSuffix})
	return string(b)
}

func (c *BuildCache) lookup(root, dir string, ov overlay, view filesys.FileSystem) ([]model.Object, []string, bool) {
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return nil, nil, false
	}
	c.mu.Lock()
	candidates := append([]*cachedBuild(nil), c.entries[overlayKey(rel, ov)]...)
	c.mu.Unlock()
next:
	for _, e := range candidates {
		for _, p := range e.probes {
			if observe(view, p.op, filepath.Join(root, p.rel)) != p.sum {
				continue next
			}
		}
		var objs []model.Object
		if err := json.Unmarshal(e.objs, &objs); err != nil {
			continue
		}
		origins := make([]string, len(e.origins))
		for i, o := range e.origins {
			if o != "" {
				origins[i] = filepath.Join(root, o)
			}
		}
		c.mu.Lock()
		c.hits++
		c.mu.Unlock()
		return objs, origins, true
	}
	return nil, nil, false
}

func (c *BuildCache) store(root, dir string, ov overlay, rec *recorder, objs []model.Object, origins []string) {
	c.mu.Lock()
	c.misses++
	c.mu.Unlock()
	rel, err := filepath.Rel(root, dir)
	if err != nil || !rec.cacheable {
		return
	}
	b, err := json.Marshal(objs)
	if err != nil {
		return
	}
	e := &cachedBuild{probes: rec.probes, objs: b, origins: make([]string, len(origins))}
	for i, o := range origins {
		if o == "" {
			continue
		}
		r, err := filepath.Rel(root, o)
		if err != nil || strings.HasPrefix(r, "..") {
			return
		}
		e.origins[i] = r
	}
	c.mu.Lock()
	c.entries[overlayKey(rel, ov)] = append(c.entries[overlayKey(rel, ov)], e)
	c.mu.Unlock()
}

// observe reduces what a filesystem view says about a path to a string.
func observe(view filesys.FileSystem, op byte, path string) string {
	switch op {
	case 'f':
		b, err := view.ReadFile(path)
		if err != nil {
			return "absent"
		}
		sum := sha256.Sum256(b)
		return hex.EncodeToString(sum[:])
	case 'd':
		names, err := view.ReadDir(path)
		if err != nil {
			return "absent"
		}
		sum := sha256.Sum256([]byte(strings.Join(names, "\x00")))
		return hex.EncodeToString(sum[:])
	default:
		switch {
		case view.IsDir(path):
			return "dir"
		case view.Exists(path):
			return "file"
		}
		return "absent"
	}
}

// recorder is a filesystem that notes everything read through it under root.
// Reads under scratch (the wrapper kustomization) are the build's own; a read
// anywhere else — a remote base kustomize cloned to a temporary directory —
// cannot be replayed, so the build is not cached.
type recorder struct {
	filesys.FileSystem
	root, scratch string
	seen          map[string]bool
	probes        []probe
	cacheable     bool
}

func newRecorder(view filesys.FileSystem, root, scratch string) *recorder {
	return &recorder{FileSystem: view, root: root, scratch: scratch, seen: map[string]bool{}, cacheable: root != ""}
}

func (r *recorder) note(op byte, path string) {
	if !r.cacheable {
		return
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		r.cacheable = false
		return
	}
	if abs == r.scratch || strings.HasPrefix(abs, r.scratch+string(filepath.Separator)) {
		return
	}
	rel, err := filepath.Rel(r.root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		r.cacheable = false
		return
	}
	if key := string(op) + rel; !r.seen[key] {
		r.seen[key] = true
		r.probes = append(r.probes, probe{op, rel, observe(r.FileSystem, op, abs)})
	}
}

func (r *recorder) Exists(path string) bool { r.note('s', path); return r.FileSystem.Exists(path) }
func (r *recorder) IsDir(path string) bool  { r.note('s', path); return r.FileSystem.IsDir(path) }

func (r *recorder) ReadFile(path string) ([]byte, error) {
	r.note('f', path)
	return r.FileSystem.ReadFile(path)
}

func (r *recorder) ReadDir(path string) ([]string, error) {
	r.note('d', path)
	return r.FileSystem.ReadDir(path)
}

func (r *recorder) CleanedAbs(path string) (filesys.ConfirmedDir, string, error) {
	r.note('s', path)
	return r.FileSystem.CleanedAbs(path)
}

// Open, Glob and Walk return more than a probe can capture.
func (r *recorder) Open(path string) (filesys.File, error) {
	r.cacheable = false
	return r.FileSystem.Open(path)
}

func (r *recorder) Glob(pattern string) ([]string, error) {
	r.cacheable = false
	return r.FileSystem.Glob(pattern)
}

func (r *recorder) Walk(path string, walkFn filepath.WalkFunc) error {
	r.cacheable = false
	return r.FileSystem.Walk(path, func(p string, info fs.FileInfo, err error) error { return walkFn(p, info, err) })
}

// Option adjusts how a tree is rendered.
type Option func(*renderer)

// WithBuildCache shares builds with other renders that use the same cache.
func WithBuildCache(c *BuildCache) Option { return func(r *renderer) { r.builds = c } }

// cachedChart is one rendered release. A chart is immutable at the revision
// its source resolved to, so the key alone decides whether it can be reused.
type cachedChart struct {
	objs     []byte
	contract *model.Contract
	notes    []string
}

func chartKey(c *model.Component, s helmSpec, kubeVersion string) string {
	if !c.External || c.SourceRevision == "" || c.FloatingRef != "" {
		return ""
	}
	b, err := json.Marshal([]any{c.SourceRoot, c.SourceRevision, s.ReleaseName, s.Namespace, s.Values, s.PostRenderers, kubeVersion})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (c *BuildCache) chart(key string) ([]model.Object, *model.Contract, []string, bool) {
	if c == nil || key == "" {
		return nil, nil, nil, false
	}
	c.mu.Lock()
	e := c.charts[key]
	c.mu.Unlock()
	if e == nil {
		return nil, nil, nil, false
	}
	var objs []model.Object
	if err := json.Unmarshal(e.objs, &objs); err != nil {
		return nil, nil, nil, false
	}
	c.mu.Lock()
	c.hits++
	c.mu.Unlock()
	return objs, e.contract, append([]string(nil), e.notes...), true
}

func (c *BuildCache) storeChart(key string, objs []model.Object, contract *model.Contract, notes []string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.misses++
	if key == "" {
		return
	}
	if b, err := json.Marshal(objs); err == nil {
		c.charts[key] = &cachedChart{objs: b, contract: contract, notes: append([]string(nil), notes...)}
	}
}
