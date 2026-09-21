package lint

import (
	"fmt"
	"sort"
	"strings"

	"github.com/phoban01/fluxlint/pkg/config"
)

// Family is the group a rule belongs to: the letter in its ID.
func (r Rule) Family() string {
	return map[byte]string{'G': "graph", 'S': "substitution", 'V': "validation", 'R': "runtime", 'C': "contracts",
		'A': "assertions", 'D': "transitions", 'X': "sources", 'T': "timing", 'O': "observed"}[r.ID[3]]
}

// Families lists the family names, in report order.
func Families() []string {
	var out []string
	seen := map[string]bool{}
	for _, r := range Rules {
		if f := r.Family(); !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	return out
}

// expand turns a rule ID, a rule name or a family name into rule IDs.
func expand(name string) []string {
	if r, ok := RuleByID(name); ok {
		return []string{r.ID}
	}
	var ids []string
	for _, r := range Rules {
		if r.Family() == strings.ToLower(name) {
			ids = append(ids, r.ID)
		}
	}
	return ids
}

// ValidateConfig resolves the rules: section against the catalogue, which only
// this package knows. A name that is not a rule is an error: a typo must not
// silently leave a rule as it was.
func ValidateConfig(cfg *config.Config) error {
	rc := &cfg.Rules
	switch rc.Default {
	case "", "all", "none":
	default:
		return fmt.Errorf("rules.default: %q is not supported; use all or none", rc.Default)
	}
	resolve := func(field string, names []string) (map[string]bool, error) {
		out := map[string]bool{}
		for _, n := range names {
			ids := expand(n)
			if len(ids) == 0 {
				return nil, fmt.Errorf("rules.%s: %q is not a rule or a family; see 'fluxlint rules'", field, n)
			}
			for _, id := range ids {
				out[id] = true
			}
		}
		return out, nil
	}
	enabled, err := resolve("enable", rc.Enable)
	if err != nil {
		return err
	}
	disabled, err := resolve("disable", rc.Disable)
	if err != nil {
		return err
	}
	// naming a family and then one of its rules on the other side is how to
	// say "all of timing except this one", so only an exact clash is an error
	for _, n := range rc.Enable {
		for _, m := range rc.Disable {
			if a, b := expand(n), expand(m); len(a) == 1 && len(b) == 1 && a[0] == b[0] {
				return fmt.Errorf("rules: %s is both enabled and disabled", a[0])
			}
		}
	}

	rc.Off = map[string]bool{}
	for _, r := range Rules {
		on := rc.Default != "none"
		// the narrower statement wins: a rule named on its own beats its family
		family := func(set map[string]bool, names []string) (named, byFamily bool) {
			if !set[r.ID] {
				return false, false
			}
			for _, n := range names {
				if rule, ok := RuleByID(n); ok && rule.ID == r.ID {
					return true, false
				}
			}
			return false, true
		}
		enNamed, enFamily := family(enabled, rc.Enable)
		disNamed, disFamily := family(disabled, rc.Disable)
		switch {
		case enNamed:
			on = true
		case disNamed:
			on = false
		case disFamily:
			on = false
		case enFamily:
			on = true
		}
		if !on {
			rc.Off[r.ID] = true
		}
	}

	rc.Level = map[string]string{}
	keys := make([]string, 0, len(rc.Severity))
	for k := range rc.Severity {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		sev := rc.Severity[key]
		rule, ok := RuleByID(key)
		if !ok {
			return fmt.Errorf("rules.severity: %q is not a rule; see 'fluxlint rules'", key)
		}
		switch Severity(sev) {
		case Error, Warning, Info:
		case Off:
			return fmt.Errorf("rules.severity: %s: use rules.disable to turn a rule off", key)
		default:
			return fmt.Errorf("rules.severity: %s: %q is not a severity; use error, warning or info", key, sev)
		}
		if _, twice := rc.Level[rule.ID]; twice {
			return fmt.Errorf("rules.severity: %s is listed twice, by ID and by name", rule.ID)
		}
		rc.Level[rule.ID] = sev
	}
	return nil
}
