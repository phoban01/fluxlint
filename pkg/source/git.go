package source

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/phoban01/fluxlint/pkg/model"
)

// gitRef is a GitRepository spec.ref reduced to what must be fetched.
type gitRef struct {
	refspec  string // what to pass to git fetch
	display  string
	floating string // non-empty: why the ref can move
	semver   string // constraint to resolve against remote tags first
}

// parseGitRef follows source-controller's precedence:
// commit > name > semver > tag > branch.
func parseGitRef(src model.Object) gitRef {
	ref := func(k string) string { return model.Str(src, "spec", "ref", k) }
	switch {
	case ref("commit") != "":
		return gitRef{refspec: ref("commit"), display: "commit " + ref("commit")}
	case ref("name") != "":
		r := gitRef{refspec: ref("name"), display: ref("name")}
		if !strings.HasPrefix(ref("name"), "refs/tags/") {
			r.floating = "ref.name " + ref("name") + " can move"
		}
		return r
	case ref("semver") != "":
		return gitRef{semver: ref("semver"), display: "semver " + ref("semver"),
			floating: "semver range " + ref("semver") + " resolves to whatever tag is newest"}
	case ref("tag") != "":
		return gitRef{refspec: "refs/tags/" + ref("tag"), display: "tag " + ref("tag")}
	default:
		branch := ref("branch")
		if branch == "" {
			branch = "master" // source-controller's default
		}
		return gitRef{refspec: "refs/heads/" + branch, display: "branch " + branch,
			floating: "branch " + branch + " moves with every push"}
	}
}

func (r *Resolver) git(ctx context.Context, src model.Object) (*Result, error) {
	url := model.Str(src, "spec", "url")
	if url == "" {
		return nil, fmt.Errorf("GitRepository %s has no spec.url", src)
	}
	ref := parseGitRef(src)
	slot := filepath.Join(r.opts.CacheDir, "git", hashOf(url, ref.display))
	metaPath := slot + ".json"

	if m, ok := readMeta(metaPath); ok && dirExists(slot) {
		if ref.floating == "" || r.opts.Mode != Refresh {
			return &Result{Dir: slot, Revision: m.Revision, Floating: ref.floating}, nil
		}
	}
	if r.opts.Mode == Offline {
		return nil, fmt.Errorf("%s (%s): %w", url, ref.display, ErrNotCached)
	}

	if ref.semver != "" {
		tag, err := newestTag(ctx, url, ref.semver)
		if err != nil {
			return nil, err
		}
		ref.refspec = "refs/tags/" + tag
	}

	if err := os.MkdirAll(filepath.Dir(slot), 0o755); err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(slot), "fetch-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)

	for _, args := range [][]string{
		{"init", "--quiet"},
		{"remote", "add", "origin", url},
		{"fetch", "--quiet", "--depth", "1", "origin", ref.refspec},
		{"-c", "advice.detachedHead=false", "checkout", "--quiet", "FETCH_HEAD"},
	} {
		if _, err := runGit(ctx, tmp, args...); err != nil {
			if strings.Contains(err.Error(), "couldn't find remote ref") {
				return nil, fmt.Errorf("%s (%s): the repository has no such ref: %w", url, ref.display, ErrNotFound)
			}
			return nil, fmt.Errorf("%s (%s): %w", url, ref.display, err)
		}
	}
	sha, err := runGit(ctx, tmp, "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	if err := os.RemoveAll(filepath.Join(tmp, ".git")); err != nil {
		return nil, err
	}

	m := meta{URL: url, Ref: ref.display, Revision: ref.display + "@sha1:" + sha, Fetched: time.Now().UTC()}
	_ = os.RemoveAll(slot) // refresh of a floating ref, or a half-written slot
	if err := os.Rename(tmp, slot); err != nil && !dirExists(slot) {
		return nil, err
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(metaPath, b, 0o644); err != nil {
		return nil, err
	}
	return &Result{Dir: slot, Revision: m.Revision, Floating: ref.floating}, nil
}

func newestTag(ctx context.Context, url, constraint string) (string, error) {
	out, err := runGit(ctx, "", "ls-remote", "--tags", "--refs", url)
	if err != nil {
		return "", fmt.Errorf("%s: %w", url, err)
	}
	var tags []string
	for _, line := range strings.Split(out, "\n") {
		if _, name, ok := strings.Cut(line, "\trefs/tags/"); ok {
			tags = append(tags, name)
		}
	}
	tag, err := newestMatching(tags, constraint)
	if err != nil {
		return "", fmt.Errorf("%s: %w", url, err)
	}
	return tag, nil
}

func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	// never block on a credential prompt; ambient credential helpers still work
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if ctx.Err() != nil {
			msg = "timed out"
		}
		if i := strings.LastIndex(msg, "\n"); i >= 0 {
			msg = msg[i+1:]
		}
		return "", fmt.Errorf("git %s: %s", args[0], msg)
	}
	return strings.TrimSpace(string(out)), nil
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}
