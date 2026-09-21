package render

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/phoban01/fluxlint/pkg/model"
	"github.com/phoban01/fluxlint/pkg/source"
)

// verifyImages asks the registry about every image of c that matches
// images.verify. Whether an image can be pulled is a fact about the registry,
// so no cluster is needed to learn it.
func (r *renderer) verifyImages(ctx context.Context, c *model.Component) {
	if r.resolver == nil || len(r.cfg.Images.Verify) == 0 {
		return
	}
	if r.verifyPatterns == nil {
		for _, p := range r.cfg.Images.Verify {
			r.verifyPatterns = append(r.verifyPatterns, imagePattern(p))
		}
	}
	var wanted []string
	for img := range images(c) {
		// an unexpanded variable is FL-S001's to report
		if strings.Contains(img, "${") {
			continue
		}
		for _, p := range r.verifyPatterns {
			if p.MatchString(img) {
				wanted = append(wanted, img)
				break
			}
		}
	}
	sort.Strings(wanted)

	problems := make([]string, len(wanted))
	notes := make([]string, len(wanted))
	var wg sync.WaitGroup
	for i, img := range wanted {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st, err := r.resolver.Image(ctx, img)
			switch {
			case errors.Is(err, source.ErrNotCached):
				notes[i] = fmt.Sprintf("image %s was not checked: running offline and it has not been seen before", img)
			case err != nil:
				notes[i] = fmt.Sprintf("image %s could not be checked: %v", img, err)
			case !st.Exists:
				problems[i] = "the registry has no such tag or digest"
			default:
				for _, want := range r.cfg.Images.Platforms {
					if len(st.Platforms) > 0 && !contains(st.Platforms, want) {
						problems[i] = fmt.Sprintf("not built for %s (the registry has %s)", want, strings.Join(st.Platforms, ", "))
					}
				}
			}
		}()
	}
	wg.Wait()
	for i, img := range wanted {
		if notes[i] != "" {
			c.RenderNotes = append(c.RenderNotes, notes[i])
		}
		if problems[i] != "" {
			if c.ImageProblems == nil {
				c.ImageProblems = map[string]string{}
			}
			c.ImageProblems[img] = problems[i]
		}
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
