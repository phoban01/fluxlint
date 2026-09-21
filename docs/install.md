# Install

## Install script

The script downloads a release binary for Linux or macOS, checks it against the
release's `checksums.txt`, and installs it to `~/.local/bin`.

```bash
curl -sSfL https://raw.githubusercontent.com/phoban01/fluxlint/main/install.sh | sh
```

Choose the directory with `-b` and the version with a tag:

```bash
curl -sSfL https://raw.githubusercontent.com/phoban01/fluxlint/main/install.sh | sh -s -- -b /usr/local/bin v0.1.0
```

| Setting | Meaning |
| --- | --- |
| `-b <dir>` or `BINDIR` | where to put the binary |
| `<tag>` or `VERSION` | the release to install; the latest if unset |
| `FLUXLINT_BASE_URL` | a mirror that holds the release files, for networks that cannot reach GitHub |

## Release archives

Archives for Linux and macOS (amd64 and arm64) are on the
[releases page](https://github.com/phoban01/fluxlint/releases), with a
`checksums.txt` beside them.

## From source

```bash
go install github.com/phoban01/fluxlint/cmd/fluxlint@latest
```

## Check the install

```bash
fluxlint version
```
