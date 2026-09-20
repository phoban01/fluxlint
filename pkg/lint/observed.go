package lint

import (
	"bufio"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/phoban01/fluxlint/pkg/model"
	"sigs.k8s.io/yaml"
)

var (
	metricLine = regexp.MustCompile(`^gotk_reconcile_duration_seconds_(sum|count)\{([^}]*)\}\s+(\S+)`)
	labelPair  = regexp.MustCompile(`(\w+)="([^"]*)"`)
)

// ParseObserved reads mean reconcile durations per component from either
//
//   - Prometheus text exposition of gotk_reconcile_duration_seconds (scrape a
//     controller's /metrics, or export the series), or
//   - a YAML map of component key to duration ("flux-system/apps: 42s",
//     "HelmRelease/cert-manager/cert-manager: 1m10s").
func ParseObserved(r io.Reader) (map[string]time.Duration, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	type agg struct{ sum, count float64 }
	series := map[string]*agg{}
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		m := metricLine.FindStringSubmatch(sc.Text())
		if m == nil {
			continue
		}
		labels := map[string]string{}
		for _, lp := range labelPair.FindAllStringSubmatch(m[2], -1) {
			labels[lp[1]] = lp[2]
		}
		ns := labels["exported_namespace"] // set when the scrape relabels the target's namespace
		if ns == "" {
			ns = labels["namespace"]
		}
		kind := model.KindKustomization
		switch labels["kind"] {
		case "HelmRelease":
			kind = model.KindHelmRelease
		case "Kustomization":
		default:
			continue
		}
		v, err := strconv.ParseFloat(m[3], 64)
		if err != nil {
			continue
		}
		key := model.KeyFor(kind, ns, labels["name"])
		if series[key] == nil {
			series[key] = &agg{}
		}
		if m[1] == "sum" {
			series[key].sum += v
		} else {
			series[key].count += v
		}
	}
	out := map[string]time.Duration{}
	for k, a := range series {
		if a.count > 0 {
			out[k] = time.Duration(a.sum / a.count * float64(time.Second))
		}
	}
	if len(out) > 0 {
		return out, nil
	}

	var plain map[string]string
	if err := yaml.Unmarshal(b, &plain); err != nil || len(plain) == 0 {
		return nil, fmt.Errorf("neither gotk_reconcile_duration_seconds metrics nor a YAML map of component to duration")
	}
	for k, v := range plain {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}
		out[k] = d
	}
	return out, nil
}
