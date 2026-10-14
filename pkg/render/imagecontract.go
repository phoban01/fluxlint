package render

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/phoban01/fluxlint/pkg/model"
)

// imagePattern compiles "registry.example.com/team/*" into a matcher. A "*"
// matches anything, including "/" and ":".
func imagePattern(p string) *regexp.Regexp {
	parts := strings.Split(p, "*")
	for i, s := range parts {
		parts[i] = regexp.QuoteMeta(s)
	}
	return regexp.MustCompile("^" + strings.Join(parts, ".*") + "$")
}

// images maps every container image c renders to the namespaces it runs in.
func images(c *model.Component) map[string][]string {
	out := map[string][]string{}
	for _, o := range c.Objects {
		var spec any
		switch {
		case o.Group() == "" && o.Kind() == "Pod":
			spec = model.Get(o, "spec")
		case o.Group() == "batch" && o.Kind() == "CronJob":
			spec = model.Get(o, "spec", "jobTemplate", "spec", "template", "spec")
		case o.Group() == "apps" || (o.Group() == "batch" && o.Kind() == "Job"):
			spec = model.Get(o, "spec", "template", "spec")
		}
		for _, list := range []string{"initContainers", "containers"} {
			for _, ctr := range model.List(spec, list) {
				img := model.Str(ctr, "image")
				if img == "" {
					continue
				}
				known := false
				for _, ns := range out[img] {
					known = known || ns == o.Namespace()
				}
				if !known {
					out[img] = append(out[img], o.Namespace())
				}
			}
		}
	}
	return out
}

// imageContracts adds the contracts attached to c's images to its own. A
// Secret or ConfigMap the contract names without a namespace is expected where
// the image runs, which need not be where the rest of the component does.
func (r *renderer) imageContracts(ctx context.Context, c *model.Component) {
	if r.resolver == nil || len(r.cfg.Contracts.Images) == 0 {
		return
	}
	if r.imagePatterns == nil {
		for _, p := range r.cfg.Contracts.Images {
			r.imagePatterns = append(r.imagePatterns, imagePattern(p))
		}
	}
	namespaces := images(c)
	var wanted []string
	for img := range namespaces {
		for _, p := range r.imagePatterns {
			if p.MatchString(img) {
				wanted = append(wanted, img)
				break
			}
		}
	}
	if len(wanted) == 0 {
		return
	}
	sort.Strings(wanted)

	found := make([]*model.Contract, len(wanted))
	notes := make([]string, len(wanted))
	var wg sync.WaitGroup
	for i, img := range wanted {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b, err := r.resolver.ImageContract(ctx, img)
			switch {
			case err != nil:
				notes[i] = fmt.Sprintf("could not look for a contract attached to image %s: %v", img, err)
			case b != nil:
				contract, err := parseContract(b)
				if err != nil {
					notes[i] = fmt.Sprintf("the contract attached to image %s is invalid and was ignored: %v", img, err)
				} else {
					found[i] = contract
				}
			}
		}()
	}
	wg.Wait()

	// the component's own contract may be shared through the build cache, so
	// merge into a copy
	merged := &model.Contract{}
	if c.Contract != nil {
		mergeContract(merged, c.Contract)
	}
	attached := false
	for i, img := range wanted {
		if notes[i] != "" {
			c.RenderNotes = append(c.RenderNotes, notes[i])
		}
		if found[i] == nil {
			continue
		}
		attached = true
		mergeContract(merged, placedOnImage(found[i], img, namespaces[img]))
	}
	if attached {
		c.Contract = merged
	}
}

// placedOnImage binds a contract fetched from img to where that image runs:
// a Secret or ConfigMap named without a namespace is expected in each
// namespace the image runs in, and a container entry that names no container
// applies to the containers that run the image.
func placedOnImage(found *model.Contract, img string, namespaces []string) *model.Contract {
	inNamespaces := func(reqs []model.ContractConfig) []model.ContractConfig {
		var out []model.ContractConfig
		for _, req := range reqs {
			if req.Namespace != "" {
				out = append(out, req)
				continue
			}
			for _, ns := range namespaces {
				placed := req
				placed.Namespace = ns
				out = append(out, placed)
			}
		}
		return out
	}
	placed := &model.Contract{}
	placed.Requires.CRDs = found.Requires.CRDs
	placed.Requires.Secrets = inNamespaces(found.Requires.Secrets)
	placed.Requires.ConfigMaps = inNamespaces(found.Requires.ConfigMaps)
	for _, ctr := range found.Requires.Containers {
		if !ctr.Selected() {
			ctr.Image = img
		}
		placed.Requires.Containers = append(placed.Requires.Containers, ctr)
	}
	return placed
}
