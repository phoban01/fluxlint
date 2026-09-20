# Run in CI

fluxlint is one binary and needs no cluster, so the job is short: install, restore the
source cache, run.

## GitLab

With `--format gitlab` fluxlint writes a Code Quality report, and GitLab shows the
findings in the merge request widget and on the diff.

```yaml
fluxlint:
  stage: test
  image: alpine:3.22
  variables:
    GIT_DEPTH: 0                     # --base needs the target branch
  before_script:
    - apk add --no-cache curl git
    - curl -sSfL https://raw.githubusercontent.com/phoban01/fluxlint/main/install.sh | sh -s -- -b /usr/local/bin
  script:
    - fluxlint check --base origin/$CI_MERGE_REQUEST_TARGET_BRANCH_NAME
        --format gitlab --output gl-code-quality-report.json
  artifacts:
    when: always
    reports:
      codequality: gl-code-quality-report.json
  cache:
    key: fluxlint-sources
    paths: [.fluxlint-cache]
  rules:
    - if: $CI_PIPELINE_SOURCE == "merge_request_event"
```

Set `sources.cacheDir: .fluxlint-cache` in `.fluxlint.yaml` so the cache lands where
GitLab can keep it.

With `--output`, the report goes to the file and the readable text goes to the job
log. The report's fingerprints leave out line numbers, so GitLab can tell a new
finding from an old one that moved.

## GitHub Actions

```yaml
name: fluxlint
on: pull_request
jobs:
  fluxlint:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with:
          fetch-depth: 0
      - run: curl -sSfL https://raw.githubusercontent.com/phoban01/fluxlint/main/install.sh | sh -s -- -b /usr/local/bin
      - uses: actions/cache@v4
        with:
          path: ~/.cache/fluxlint
          key: fluxlint-sources
      - run: fluxlint check --base origin/${{ github.base_ref }} --format github
```

`--format github` prints workflow annotations, which GitHub shows on the diff. For
code scanning, write SARIF and upload it:

```yaml
      - run: fluxlint check --format sarif --output fluxlint.sarif
      - uses: github/codeql-action/upload-sarif@v3
        if: always()
        with:
          sarif_file: fluxlint.sarif
```

## Private sources

fluxlint uses the credentials the job already has.

| Source | Credentials |
| --- | --- |
| Git | `git` credential helpers and `insteadOf` rules |
| OCI | the Docker config, as written by `docker login` |
| HTTP Helm repository | `$NETRC` or `~/.netrc` |

In GitLab, the job token covers all three:

```yaml
  before_script:
    - git config --global url."https://gitlab-ci-token:${CI_JOB_TOKEN}@gitlab.example.com/".insteadOf "https://gitlab.example.com/"
    - echo "machine gitlab.example.com login gitlab-ci-token password ${CI_JOB_TOKEN}" > ~/.netrc
    - mkdir -p ~/.docker
    - echo "{\"auths\":{\"registry.example.com\":{\"auth\":\"$(printf 'gitlab-ci-token:%s' "$CI_JOB_TOKEN" | base64 | tr -d '\n')\"}}}" > ~/.docker/config.json
```

If the job has already checked out a sibling repository, skip the fetch and point
the source at the directory:

```yaml
# .fluxlint.yaml
sources:
  overrides:
    - {kind: GitRepository, name: my-operator, path: ../my-operator}
```

## Pin what you run

A floating ref, such as a branch or a semver range, means Flux can apply something new
without a commit to your repository. fluxlint reports these as `FL-X002`. In CI,
results are reproducible only for pinned refs. Pass `--refresh` when you want floating
refs resolved again.
