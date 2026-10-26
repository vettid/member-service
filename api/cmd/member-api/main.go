// Command member-api is the backend of member.vettid.org: sessions,
// roster, proposals, the beta program and comments. It integrates with
// the VettID service vault through its SDK and webhooks, exactly as a
// third-party application would (MEMBER-SERVICE-PLAN §3, §7.3).
//
// Phase 0: only the version command exists.
package main

import (
	"flag"
	"fmt"
	"os"
)

// version is set at build time with -ldflags '-X main.version=...'.
var version = "dev"

func main() {
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "usage: member-api version\n")
	}
	flag.Parse()
	switch flag.Arg(0) {
	case "version":
		fmt.Println(version)
	default:
		flag.Usage()
		os.Exit(2)
	}
}
