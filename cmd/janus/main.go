package main

import (
	"flag"
	"fmt"
	"os"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	showVersion := flag.Bool("version", false, "print version")
	flag.Parse()

	if *showVersion {
		fmt.Printf("janus %s (commit %s, built %s)\n", version, commit, date)
		return
	}

	fmt.Fprintln(os.Stderr, "janus v2 skeleton: see docs/REFACTOR.md and docs/STRUCTURE.md")
}
