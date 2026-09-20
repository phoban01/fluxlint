package report

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/phoban01/fluxlint/pkg/lint"
	"github.com/phoban01/fluxlint/pkg/model"
)

func sample() Summary {
	return Summary{Results: []*lint.Result{{
		Tree: &model.Tree{Entrypoint: "clusters/prod"},
		Findings: []lint.Finding{
			{Rule: "FL-G004", Name: "missing-namespace", Severity: lint.Error, Entrypoint: "clusters/prod",
				Component: "flux-system/apps", Object: "ConfigMap/nowhere/x", File: "apps/x.yaml", Line: 7,
				Message: `namespace "nowhere" is not created`, Detail: []string{"a: b, c"}},
			{Rule: "FL-T007", Name: "retry-cliff", Severity: lint.Warning, Entrypoint: "clusters/prod", Message: "no retryInterval"},
			{Rule: "FL-T001", Name: "critical-path", Severity: lint.Info, Entrypoint: "clusters/prod", Message: "bound is 10m"},
		},
	}}}
}

func TestGitLabCodeQuality(t *testing.T) {
	var buf bytes.Buffer
	if err := GitLab(&buf, sample(), false); err != nil {
		t.Fatal(err)
	}
	var issues []struct {
		Description, CheckName, Fingerprint, Severity string
		Location                                      struct {
			Path  string
			Lines struct{ Begin int }
		}
	}
	if err := json.Unmarshal(bytes.ReplaceAll(buf.Bytes(), []byte("check_name"), []byte("CheckName")), &issues); err != nil {
		t.Fatal(err)
	}
	if len(issues) != 2 {
		t.Fatalf("suggestions are left out unless -v: %d issues", len(issues))
	}
	first := issues[0]
	if first.CheckName != "FL-G004" || first.Severity != "critical" || first.Location.Path != "apps/x.yaml" || first.Location.Lines.Begin != 7 {
		t.Errorf("unexpected issue: %+v", first)
	}
	// a finding without a file still needs a path GitLab accepts
	if issues[1].Location.Path != "clusters/prod" || issues[1].Location.Lines.Begin != 1 || issues[1].Severity != "major" {
		t.Errorf("fallback location: %+v", issues[1])
	}
	if len(first.Fingerprint) != 64 || first.Fingerprint == issues[1].Fingerprint {
		t.Errorf("fingerprints must be stable and distinct")
	}
	// the fingerprint must not move when only the line does
	moved := sample()
	moved.Results[0].Findings[0].Line = 99
	var again bytes.Buffer
	_ = GitLab(&again, moved, false)
	if !strings.Contains(again.String(), first.Fingerprint) {
		t.Errorf("fingerprint changed with the line number")
	}
}

func TestSARIF(t *testing.T) {
	var buf bytes.Buffer
	if err := SARIF(&buf, sample(), true); err != nil {
		t.Fatal(err)
	}
	var log struct {
		Version string
		Runs    []struct {
			Tool struct {
				Driver struct{ Rules []struct{ ID string } }
			}
			Results []struct {
				RuleID, Level string
				Locations     []struct {
					PhysicalLocation struct {
						ArtifactLocation struct{ URI string }
						Region           struct{ StartLine int }
					}
				}
			}
		}
	}
	if err := json.Unmarshal(buf.Bytes(), &log); err != nil {
		t.Fatal(err)
	}
	run := log.Runs[0]
	if log.Version != "2.1.0" || len(run.Tool.Driver.Rules) != len(lint.Rules) || len(run.Results) != 3 {
		t.Fatalf("version %q, %d rules, %d results", log.Version, len(run.Tool.Driver.Rules), len(run.Results))
	}
	if r := run.Results[0]; r.Level != "error" || r.Locations[0].PhysicalLocation.ArtifactLocation.URI != "apps/x.yaml" || r.Locations[0].PhysicalLocation.Region.StartLine != 7 {
		t.Errorf("result: %+v", r)
	}
	if run.Results[2].Level != "note" {
		t.Errorf("info maps to note: %+v", run.Results[2])
	}
}

func TestGitHubAnnotations(t *testing.T) {
	var buf bytes.Buffer
	GitHub(&buf, sample(), false)
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("one command per finding: %q", lines)
	}
	want := `::error file=apps/x.yaml,line=7,title=FL-G004 missing-namespace::flux-system/apps ConfigMap/nowhere/x: namespace "nowhere" is not created%0Aa: b, c`
	if lines[0] != want {
		t.Errorf("got  %s\nwant %s", lines[0], want)
	}
}
