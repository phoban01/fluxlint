package lint

import (
	"fmt"
	"sort"

	"github.com/phoban01/fluxlint/pkg/config"
)

// ValidateConfig checks the parts of the configuration only this package can:
// a rule override must name a rule that exists, by ID or by name, and give it
// a severity that exists. A typo must not silently leave a rule as it was.
// Overrides given by name are rewritten to the rule's ID.
func ValidateConfig(cfg *config.Config) error {
	keys := make([]string, 0, len(cfg.Rules))
	for k := range cfg.Rules {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		sev := cfg.Rules[key]
		rule, ok := RuleByID(key)
		if !ok {
			return fmt.Errorf("rules: %q is not a rule; see 'fluxlint rules'", key)
		}
		switch Severity(sev) {
		case Error, Warning, Info, Off:
		default:
			return fmt.Errorf("rules: %s: %q is not a severity; use error, warning, info or off", key, sev)
		}
		if key != rule.ID {
			if _, both := cfg.Rules[rule.ID]; both {
				return fmt.Errorf("rules: %s and %s are the same rule", rule.ID, key)
			}
			delete(cfg.Rules, key)
			cfg.Rules[rule.ID] = sev
		}
	}
	return nil
}
