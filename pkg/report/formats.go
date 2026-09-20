package report

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/phoban01/fluxlint/pkg/lint"
)

// location gives every finding somewhere to live: formats that render inline
// in a merge request need a path even for repository-wide findings.
func location(f lint.Finding) (string, int) {
	if f.File != "" {
		return f.File, max(f.Line, 1)
	}
	return f.Entrypoint, 1
}

func headline(f lint.Finding) string {
	where := strings.TrimSpace(f.Component + " " + f.Object)
	if where != "" {
		where += ": "
	}
	return where + f.Message
}

func withDetail(f lint.Finding) string {
	if len(f.Detail) == 0 {
		return headline(f)
	}
	return headline(f) + "\n" + strings.Join(f.Detail, "\n")
}

// reportable drops suggestions unless asked for: review tools present every
// entry as a defect.
func reportable(s Summary, verbose bool) []lint.Finding {
	var out []lint.Finding
	for _, r := range s.Results {
		for _, f := range r.Findings {
			if f.Existing || (f.Severity == lint.Info && !verbose) {
				continue
			}
			out = append(out, f)
		}
	}
	return out
}

// GitLab writes a Code Quality report (the Code Climate subset GitLab reads),
// for `artifacts:reports:codequality`.
func GitLab(w io.Writer, s Summary, verbose bool) error {
	type lines struct {
		Begin int `json:"begin"`
	}
	type loc struct {
		Path  string `json:"path"`
		Lines lines  `json:"lines"`
	}
	type issue struct {
		Description string `json:"description"`
		CheckName   string `json:"check_name"`
		Fingerprint string `json:"fingerprint"`
		Severity    string `json:"severity"`
		Location    loc    `json:"location"`
	}
	severity := map[lint.Severity]string{lint.Error: "critical", lint.Warning: "major", lint.Info: "info"}
	issues := []issue{}
	for _, f := range reportable(s, verbose) {
		path, line := location(f)
		// stable across line moves and unrelated edits, so GitLab can tell
		// new findings from existing ones
		issues = append(issues, issue{
			Description: fmt.Sprintf("%s %s: %s", f.Rule, f.Name, headline(f)),
			CheckName:   f.Rule,
			Fingerprint: f.Fingerprint(),
			Severity:    severity[f.Severity],
			Location:    loc{Path: path, Lines: lines{Begin: line}},
		})
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(issues)
}

// SARIF writes a SARIF 2.1.0 log, for GitHub code scanning and most IDEs.
func SARIF(w io.Writer, s Summary, verbose bool) error {
	type msg struct {
		Text string `json:"text"`
	}
	type rule struct {
		ID               string `json:"id"`
		Name             string `json:"name"`
		ShortDescription msg    `json:"shortDescription"`
		FullDescription  msg    `json:"fullDescription"`
		DefaultConfig    struct {
			Level string `json:"level"`
		} `json:"defaultConfiguration"`
	}
	type region struct {
		StartLine int `json:"startLine"`
	}
	type physical struct {
		ArtifactLocation struct {
			URI string `json:"uri"`
		} `json:"artifactLocation"`
		Region region `json:"region"`
	}
	type result struct {
		RuleID    string `json:"ruleId"`
		Level     string `json:"level"`
		Message   msg    `json:"message"`
		Locations []struct {
			PhysicalLocation physical `json:"physicalLocation"`
		} `json:"locations"`
	}
	level := map[lint.Severity]string{lint.Error: "error", lint.Warning: "warning", lint.Info: "note"}

	var rules []rule
	for _, r := range lint.Rules {
		sr := rule{ID: r.ID, Name: r.Name, ShortDescription: msg{r.Name}, FullDescription: msg{r.Help}}
		sr.DefaultConfig.Level = level[r.Severity]
		rules = append(rules, sr)
	}
	results := []result{}
	for _, f := range reportable(s, verbose) {
		path, line := location(f)
		res := result{RuleID: f.Rule, Level: level[f.Severity], Message: msg{withDetail(f)}}
		var p physical
		p.ArtifactLocation.URI = path
		p.Region.StartLine = line
		res.Locations = append(res.Locations, struct {
			PhysicalLocation physical `json:"physicalLocation"`
		}{p})
		results = append(results, res)
	}

	out := map[string]any{
		"$schema": "https://json.schemastore.org/sarif-2.1.0.json",
		"version": "2.1.0",
		"runs": []any{map[string]any{
			"tool": map[string]any{"driver": map[string]any{
				"name":           "fluxlint",
				"informationUri": "https://github.com/phoban01/fluxlint",
				"rules":          rules,
			}},
			"results": results,
		}},
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// GitHub writes workflow commands, which GitHub Actions turns into inline
// annotations on the pull request.
func GitHub(w io.Writer, s Summary, verbose bool) {
	command := map[lint.Severity]string{lint.Error: "error", lint.Warning: "warning", lint.Info: "notice"}
	escape := strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A")
	property := strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A", ":", "%3A", ",", "%2C")
	for _, f := range reportable(s, verbose) {
		path, line := location(f)
		fmt.Fprintf(w, "::%s file=%s,line=%d,title=%s::%s\n", command[f.Severity],
			property.Replace(path), line, property.Replace(f.Rule+" "+f.Name), escape.Replace(withDetail(f)))
	}
}
