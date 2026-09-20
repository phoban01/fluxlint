package main

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// Set by the release build (-ldflags -X); `go install` builds fall back to
// the module version recorded in the binary.
var (
	version = ""
	commit  = ""
	date    = ""
)

func versionString() string {
	v := version
	if v == "" {
		v = "dev"
		if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			v = bi.Main.Version
		}
	}
	out := "fluxlint " + v
	if commit != "" {
		out += fmt.Sprintf(" (%s, %s)", commit, date)
	}
	return out + " " + runtime.Version() + " " + runtime.GOOS + "/" + runtime.GOARCH
}
