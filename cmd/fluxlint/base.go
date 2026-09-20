package main

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// checkoutBase materialises the repository at a Git ref into a temporary
// directory, without touching the working tree or the index.
func checkoutBase(repo, ref string) (string, func(), error) {
	dir, err := os.MkdirTemp("", "fluxlint-base-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { os.RemoveAll(dir) }

	cmd := exec.Command("git", "-C", repo, "archive", "--format=tar", ref)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		cleanup()
		return "", nil, err
	}
	if err := cmd.Start(); err != nil {
		cleanup()
		return "", nil, err
	}
	extractErr := untar(out, dir)
	if err := cmd.Wait(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("git archive %s: %s", ref, strings.TrimSpace(stderr.String()))
	}
	if extractErr != nil {
		cleanup()
		return "", nil, extractErr
	}
	return dir, cleanup, nil
}

func untar(r io.Reader, dst string) error {
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		target := filepath.Join(dst, filepath.FromSlash(h.Name))
		if rel, err := filepath.Rel(dst, target); err != nil || strings.HasPrefix(rel, "..") {
			return fmt.Errorf("archive entry %q escapes the extraction directory", h.Name)
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
			_, err = io.Copy(f, tr)
			f.Close()
			if err != nil {
				return err
			}
		}
	}
}
