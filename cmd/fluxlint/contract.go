package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/phoban01/fluxlint/pkg/model"
	"github.com/phoban01/fluxlint/pkg/render"
	"github.com/phoban01/fluxlint/pkg/source"
	"sigs.k8s.io/yaml"
)

const contractUsage = `Usage:
  fluxlint contract push [-f file] <image>   attach a contract to an image in a registry
  fluxlint contract pull <image>             print the contract attached to an image

The contract is stored as an OCI artifact that refers to the image, so it travels
with the image and cannot belong to another version. Push it from the pipeline
that pushes the image. Credentials come from your Docker config.
`

func contractCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, contractUsage)
		return 2
	}
	fs := flag.NewFlagSet("contract "+args[0], flag.ContinueOnError)
	file := fs.String("f", render.ContractFile, "the contract to push")
	timeout := fs.Duration("timeout", time.Minute, "how long the registry may take")
	fs.Usage = func() { fmt.Fprint(os.Stderr, contractUsage); fs.PrintDefaults() }
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	image := fs.Arg(0)
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	switch args[0] {
	case "push":
		b, err := os.ReadFile(*file)
		if err == nil {
			// never publish a contract fluxlint itself would refuse to read
			err = yaml.UnmarshalStrict(b, &model.Contract{})
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %s: %v\n", *file, err)
			return 2
		}
		ref, err := source.PushContract(ctx, image, b)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 2
		}
		fmt.Printf("attached %s to %s\n%s\n", *file, image, ref)
	case "pull":
		b, err := source.FetchContract(ctx, image)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 2
		}
		if b == nil {
			fmt.Fprintf(os.Stderr, "%s has no contract attached\n", image)
			return 1
		}
		fmt.Print(string(b))
	default:
		fmt.Fprint(os.Stderr, contractUsage)
		return 2
	}
	return 0
}
