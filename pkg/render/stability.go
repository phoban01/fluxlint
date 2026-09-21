package render

import (
	"regexp"
	"sort"
	"strings"

	"github.com/phoban01/fluxlint/pkg/model"
	"helm.sh/helm/v3/pkg/chart"
	"sigs.k8s.io/yaml"
)

// Template functions whose result differs from one render to the next.
var unstableCall = regexp.MustCompile(`\{\{[^}]*\b(randAlphaNum|randAlpha|randNumeric|randAscii|randBytes|uuidv4|now|genCA|genCAWithKey|genSelfSignedCert|genSelfSignedCertWithKey|genSignedCert|genSignedCertWithKey|genPrivateKey|htpasswd|bcrypt|encryptAES)\b`)

// A template that calls lookup usually does so to keep what is already in
// the cluster, which no offline render can show.
var lookupCall = regexp.MustCompile(`\{\{[^}]*\blookup\b`)

var sourceLine = regexp.MustCompile(`(?m)^# Source: (.+)$`)

// templatesOf maps the path Helm prints in "# Source:" to the template text.
func templatesOf(ch *chart.Chart, prefix string, into map[string]string) {
	base := prefix + ch.Name() + "/"
	for _, t := range ch.Templates {
		into[base+t.Name] = string(t.Data)
	}
	for _, dep := range ch.Dependencies() {
		templatesOf(dep, base+"charts/", into)
	}
}

// unstableObjects renders the release a second time and names the objects
// that came out differently. Helm renders again on every upgrade, so such an
// object changes whenever the release is upgraded, whatever the upgrade was
// for. The second render only happens when some template calls a function
// that can differ.
func unstableObjects(ch *chart.Chart, s helmSpec, kubeVersion, first string) []string {
	templates := map[string]string{}
	templatesOf(ch, "", templates)
	suspect := false
	for _, text := range templates {
		suspect = suspect || unstableCall.MatchString(text)
	}
	if !suspect {
		return nil
	}
	second, err := helmManifest(ch, s, kubeVersion)
	if err != nil {
		return nil
	}
	a, b := docsByID(first, s.Namespace), docsByID(second, s.Namespace)
	var out []string
	for id, da := range a {
		db, ok := b[id]
		if !ok || da.text == db.text {
			continue
		}
		if lookupCall.MatchString(templates[da.source]) {
			continue
		}
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

type renderedDoc struct{ text, source string }

func docsByID(manifest, namespace string) map[string]renderedDoc {
	out := map[string]renderedDoc{}
	for _, doc := range strings.Split("\n"+manifest, "\n---") {
		var o model.Object
		if err := yaml.Unmarshal([]byte(doc), &o); err != nil || o == nil || o.Kind() == "" {
			continue
		}
		if o.Namespace() == "" && !clusterScoped(o) {
			if meta, ok := o["metadata"].(map[string]any); ok {
				meta["namespace"] = namespace
			}
		}
		d := renderedDoc{text: doc}
		if m := sourceLine.FindStringSubmatch(doc); m != nil {
			d.source = strings.TrimSpace(m[1])
		}
		out[o.ID()] = d
	}
	return out
}
