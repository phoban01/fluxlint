# Command line

```
fluxlint check   [flags] [entrypoint ...]   analyse the repository
fluxlint graph   [flags] [entrypoint]       print the dependency graph
fluxlint explain <rule>                     describe a rule
fluxlint rules                              list all rules
fluxlint contract push|pull <image>         attach a contract to an image, or read one
fluxlint version                            print the version
```

An entrypoint is the directory a cluster's bootstrap Kustomization points at, relative
to the repository root. Entrypoints given on the command line replace the ones in
`.fluxlint.yaml`.

## check

| Flag | Default | Meaning |
| --- | --- | --- |
| `--repo` | `.` | the repository root |
| `--config` | `<repo>/.fluxlint.yaml` | the config file. A missing file means defaults. |
| `--base` | | a Git ref to compare with, such as `origin/main`. Only new findings fail the run, and the transition is analysed. See [Check merge requests](../guides/merge-requests.md). |
| `--format` | `text` | `text`, `json`, `gitlab` (Code Quality), `sarif` or `github` (workflow annotations) |
| `--output` | | write the report to this file and print the text report to stdout |
| `--fail-on` | `error` | the lowest severity that fails the run: `error` or `warning` |
| `-v` | | list suggestions (`info`) in full |
| `--observed` | | a file of observed reconcile durations. See [Shorten bootstrap time](../guides/timing.md). |
| `--cluster-state` | | the Flux objects of a real cluster at this commit (`kubectl get kustomizations.kustomize.toolkit.fluxcd.io,helmreleases.helm.toolkit.fluxcd.io -A -o json`). Every failure that no finding predicted is reported as `FL-O001`. Takes one entrypoint. See [Run beside a cluster test](../guides/cluster-tests.md). |
| `--offline` | | never use the network. A source that is not cached is reported as `FL-X001`. |
| `--refresh` | | resolve floating refs again: branches, semver ranges, `latest` |
| `--cache-dir` | the user cache directory | where fetched sources are kept. `sources.cacheDir` in the config sets it too. |

Several entrypoints are analysed in parallel and share their builds.

### Exit codes

| Code | Meaning |
| --- | --- |
| `0` | no findings at or above `--fail-on` |
| `1` | findings at or above `--fail-on` |
| `2` | fluxlint could not run: bad flags, an unreadable config, no entrypoint |

### JSON output

`--format json` prints one document:

```json
{
  "entrypoints": [
    {
      "entrypoint": "clusters/production",
      "findings": [
        {
          "rule": "FL-G004",
          "name": "missing-namespace",
          "severity": "error",
          "entrypoint": "clusters/production",
          "component": "flux-system/apps",
          "object": "ConfigMap/team-a/settings",
          "file": "apps/settings.yaml",
          "line": 4,
          "message": "namespace \"team-a\" is not created by any component"
        }
      ],
      "timing": {
        "bound": 300000000000,
        "criticalPath": [
          {"event": "start(flux-system/apps)", "at": 0, "added": 0, "reason": "created by parent"},
          {"event": "ready(flux-system/apps)", "at": 300000000000, "added": 300000000000, "reason": "apply and health timeout"}
        ]
      }
    }
  ],
  "elapsedMs": 940
}
```

Durations are nanoseconds. Some findings carry a `detail` list of extra lines. With
`--base`, findings that also exist at the base carry
`"existing": true`.

## graph

`graph` takes the same flags as `check` for finding the repository, the config and the
sources. `--format` is `dot` (the default) or `mermaid`. It draws one entrypoint, which
you name unless the config lists exactly one. See [Draw the graph](../guides/graph.md).

## explain and rules

`fluxlint rules` lists every rule with its default severity. `fluxlint explain FL-G002`
prints what a rule means and what to do about it. The same text is in the
[rules reference](rules.md).

## contract

```
fluxlint contract push [-f file] <image>
fluxlint contract pull <image>
```

`push` attaches a [contract](../guides/contracts.md) to an image that is already in a
registry. `-f` defaults to `fluxlint-contract.yaml`. `pull` prints the contract
attached to an image, and exits `1` when there is none. `--timeout` bounds how long the
registry may take, and defaults to one minute.
