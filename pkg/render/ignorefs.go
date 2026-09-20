package render

import (
	"io/fs"
	"os"
	"path/filepath"

	"sigs.k8s.io/kustomize/kyaml/filesys"
)

// ignoreFS is the disk as kustomize-controller sees it: paths the source left
// out of its artifact do not exist. A kustomization.yaml that lists such a file
// fails to build here exactly as it does in the cluster.
type ignoreFS struct {
	filesys.FileSystem
	ignored func(path string, isDir bool) bool
}

func newIgnoreFS(ignored func(string, bool) bool) filesys.FileSystem {
	disk := filesys.MakeFsOnDisk()
	if ignored == nil {
		return disk
	}
	return ignoreFS{FileSystem: disk, ignored: ignored}
}

// hidden reports whether path, or a directory above it, is ignored.
func (f ignoreFS) hidden(path string) bool {
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	if f.ignored(abs, f.FileSystem.IsDir(abs)) {
		return true
	}
	for dir := filepath.Dir(abs); dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
		if f.ignored(dir, true) {
			return true
		}
	}
	return false
}

func notExist(op, path string) error {
	return &fs.PathError{Op: op, Path: path, Err: os.ErrNotExist}
}

func (f ignoreFS) Exists(path string) bool { return !f.hidden(path) && f.FileSystem.Exists(path) }
func (f ignoreFS) IsDir(path string) bool  { return !f.hidden(path) && f.FileSystem.IsDir(path) }

func (f ignoreFS) Open(path string) (filesys.File, error) {
	if f.hidden(path) {
		return nil, notExist("open", path)
	}
	return f.FileSystem.Open(path)
}

func (f ignoreFS) ReadFile(path string) ([]byte, error) {
	if f.hidden(path) {
		return nil, notExist("open", path)
	}
	return f.FileSystem.ReadFile(path)
}

func (f ignoreFS) CleanedAbs(path string) (filesys.ConfirmedDir, string, error) {
	if f.hidden(path) {
		return "", "", notExist("stat", path)
	}
	return f.FileSystem.CleanedAbs(path)
}

func (f ignoreFS) ReadDir(path string) ([]string, error) {
	if f.hidden(path) {
		return nil, notExist("open", path)
	}
	names, err := f.FileSystem.ReadDir(path)
	if err != nil {
		return nil, err
	}
	kept := names[:0]
	for _, n := range names {
		if !f.hidden(filepath.Join(path, n)) {
			kept = append(kept, n)
		}
	}
	return kept, nil
}

func (f ignoreFS) Glob(pattern string) ([]string, error) {
	matches, err := f.FileSystem.Glob(pattern)
	if err != nil {
		return nil, err
	}
	kept := matches[:0]
	for _, m := range matches {
		if !f.hidden(m) {
			kept = append(kept, m)
		}
	}
	return kept, nil
}

func (f ignoreFS) Walk(path string, walkFn filepath.WalkFunc) error {
	return f.FileSystem.Walk(path, func(p string, info fs.FileInfo, err error) error {
		if err == nil && f.hidden(p) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		return walkFn(p, info, err)
	})
}
