package main

import (
	"fmt"
	"os"

	"github.com/kube-dosh/kube-do/manifests"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: ha-image-source <component> <arch>")
		os.Exit(2)
	}
	component, arch := os.Args[1], os.Args[2]
	names, err := manifests.Names()
	if err != nil || len(names) == 0 {
		fmt.Fprintf(os.Stderr, "list embedded builds: %v\n", err)
		os.Exit(1)
	}
	m, err := manifests.Get(names[len(names)-1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "load newest Build: %v\n", err)
		os.Exit(1)
	}
	matches := 0
	for _, item := range m.Spec.Installer.Binaries {
		if item.Name != component || item.Arch != arch {
			continue
		}
		matches++
		if !item.IsBuilt() {
			fmt.Fprintf(os.Stderr, "%s/%s is not a source-built artifact\n", component, arch)
			os.Exit(1)
		}
		fmt.Printf("version=%s\nurl=%s\nsha256=%s\n", item.Version, item.URL, item.SourceSHA256)
	}
	if matches != 1 {
		fmt.Fprintf(os.Stderr, "expected one %s source for %s, found %d\n", component, arch, matches)
		os.Exit(1)
	}
}
