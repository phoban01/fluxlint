package source

import (
	"os"
	"path/filepath"
	"strings"
)

// netrcCredentials returns the login for host from $NETRC or ~/.netrc — the
// same file git and curl read, and the usual way to hand a CI job token to
// tools. Flux itself authenticates with a Secret in the cluster, which a
// static analysis cannot and should not read.
func netrcCredentials(host string) (login, password string, ok bool) {
	path := os.Getenv("NETRC")
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", "", false
		}
		path = filepath.Join(home, ".netrc")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", "", false
	}
	// strip the port: netrc entries are per machine
	if h, _, found := strings.Cut(host, ":"); found {
		host = h
	}
	var machine, l, p string
	var def struct{ l, p string }
	flush := func() bool { return machine == host && l != "" }
	fields := strings.Fields(string(b))
	for i := 0; i < len(fields); i++ {
		switch fields[i] {
		case "machine", "default":
			if flush() {
				return l, p, true
			}
			if machine == "\x00default" {
				def.l, def.p = l, p
			}
			l, p = "", ""
			if fields[i] == "default" {
				machine = "\x00default"
			} else if i+1 < len(fields) {
				i++
				machine = fields[i]
			}
		case "login":
			if i+1 < len(fields) {
				i++
				l = fields[i]
			}
		case "password":
			if i+1 < len(fields) {
				i++
				p = fields[i]
			}
		}
	}
	if flush() {
		return l, p, true
	}
	if machine == "\x00default" {
		def.l, def.p = l, p
	}
	return def.l, def.p, def.l != ""
}
