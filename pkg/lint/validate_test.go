package lint_test

import (
	"strings"
	"testing"

	"github.com/phoban01/fluxlint/pkg/config"
	"github.com/phoban01/fluxlint/pkg/lint"
)

func rules(t *testing.T, rc config.Rules) (*config.Config, error) {
	t.Helper()
	cfg := config.Default()
	cfg.Rules = rc
	return cfg, lint.ValidateConfig(cfg)
}

func TestRuleSelection(t *testing.T) {
	timing := 0
	for _, r := range lint.Rules {
		if r.Family() == "timing" {
			timing++
		}
	}
	for name, tc := range map[string]struct {
		rules   config.Rules
		on, off []string
		offN    int
	}{
		"everything by default": {config.Rules{}, []string{"FL-G002", "FL-T007"}, nil, 0},
		"disable by ID and by name": {config.Rules{Disable: []string{"FL-T007", "positional-patch"}},
			[]string{"FL-G002"}, []string{"FL-T007", "FL-R003"}, 2},
		"disable a family": {config.Rules{Disable: []string{"timing"}}, []string{"FL-G002"}, []string{"FL-T001", "FL-T100"}, timing},
		"a family except one rule": {config.Rules{Disable: []string{"timing"}, Enable: []string{"critical-path"}},
			[]string{"FL-T001"}, []string{"FL-T007"}, timing - 1},
		"none, then a few": {config.Rules{Default: "none", Enable: []string{"FL-G002", "graph"}},
			[]string{"FL-G002", "FL-G001"}, []string{"FL-T007", "FL-R001"}, -1},
		"none, a family minus one rule": {config.Rules{Default: "none", Enable: []string{"timing"}, Disable: []string{"retry-cliff"}},
			[]string{"FL-T001"}, []string{"FL-T007", "FL-G002"}, len(lint.Rules) - timing + 1},
	} {
		cfg, err := rules(t, tc.rules)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		for _, id := range tc.on {
			if cfg.Rules.Off[id] {
				t.Errorf("%s: %s should be on", name, id)
			}
		}
		for _, id := range tc.off {
			if !cfg.Rules.Off[id] {
				t.Errorf("%s: %s should be off", name, id)
			}
		}
		if tc.offN >= 0 && len(cfg.Rules.Off) != tc.offN {
			t.Errorf("%s: %d rules off, want %d", name, len(cfg.Rules.Off), tc.offN)
		}
	}
}

func TestSeverityOverride(t *testing.T) {
	cfg, err := rules(t, config.Rules{Severity: map[string]string{"positional-patch": "error", "FL-T007": "info"}})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Rules.Level["FL-R003"] != "error" || cfg.Rules.Level["FL-T007"] != "info" {
		t.Errorf("an override by name applies to the rule's ID: %v", cfg.Rules.Level)
	}
}

// A typo must not silently leave a rule as it was.
func TestRuleSelectionErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		rules config.Rules
		want  string
	}{
		"unknown default":       {config.Rules{Default: "standard"}, "use all or none"},
		"unknown rule":          {config.Rules{Disable: []string{"FL-T07"}}, `"FL-T07" is not a rule or a family`},
		"unknown enabled rule":  {config.Rules{Enable: []string{"timings"}}, "is not a rule or a family"},
		"enabled and disabled":  {config.Rules{Enable: []string{"FL-T007"}, Disable: []string{"retry-cliff"}}, "both enabled and disabled"},
		"unknown severity":      {config.Rules{Severity: map[string]string{"FL-T007": "fatal"}}, "is not a severity"},
		"off is not a severity": {config.Rules{Severity: map[string]string{"FL-T007": "off"}}, "use rules.disable"},
		"severity of a family":  {config.Rules{Severity: map[string]string{"timing": "error"}}, "is not a rule"},
		"same rule twice":       {config.Rules{Severity: map[string]string{"FL-T007": "info", "retry-cliff": "error"}}, "listed twice"},
	} {
		if _, err := rules(t, tc.rules); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want an error containing %q", name, err, tc.want)
		}
	}
}

// Selection must reach the findings, not just the config.
func TestDisabledRuleReportsNothing(t *testing.T) {
	if len(find(analyse(t, "timing", nil), "FL-T004")) == 0 {
		t.Fatal("the fixture should produce FL-T004 by default")
	}
	cfg, err := rules(t, config.Rules{Disable: []string{"unjustified-dependency"}, Severity: map[string]string{"FL-T005": "error"}})
	if err != nil {
		t.Fatal(err)
	}
	r := analyse(t, "timing", cfg)
	if got := find(r, "FL-T004"); len(got) != 0 {
		t.Errorf("disabled rule still reports: %+v", got)
	}
	if got := find(r, "FL-T005"); len(got) != 1 || got[0].Severity != lint.Error {
		t.Errorf("severity override: %+v", got)
	}
}
