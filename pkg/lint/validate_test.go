package lint_test

import (
	"strings"
	"testing"

	"github.com/phoban01/fluxlint/pkg/config"
	"github.com/phoban01/fluxlint/pkg/lint"
)

func TestValidateConfigRules(t *testing.T) {
	cfg := config.Default()
	cfg.Rules = map[string]string{"FL-T007": "off", "positional-patch": "error"}
	if err := lint.ValidateConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Rules["FL-R003"] != "error" || len(cfg.Rules) != 2 {
		t.Errorf("an override by name applies to the rule's ID: %v", cfg.Rules)
	}

	for name, tc := range map[string]struct {
		rules map[string]string
		want  string
	}{
		"unknown rule":     {map[string]string{"FL-T07": "off"}, "is not a rule"},
		"unknown severity": {map[string]string{"FL-T007": "of"}, "is not a severity"},
		"same rule twice":  {map[string]string{"FL-T007": "off", "retry-cliff": "error"}, "same rule"},
	} {
		cfg := config.Default()
		cfg.Rules = tc.rules
		if err := lint.ValidateConfig(cfg); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want an error containing %q", name, err, tc.want)
		}
	}
}
