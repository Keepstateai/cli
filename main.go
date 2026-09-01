// ks — the KeepState thin client. A key to a building only we operate: every
// verb terminates at the KeepState control plane over HTTPS. No engine code
// lives here. MIT licensed; built in public CI; releases carry SHA256SUMS and
// provenance attestations, and `ks update` verifies before trusting.
package main

import (
	"fmt"
	"os"
)

// version is stamped by CI: -ldflags "-X main.version=v0.1.0"
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	verb := os.Args[1]
	switch verb {
	case "login":
		if err := runLogin(); err != nil {
			die(err)
		}
	case "logout":
		if err := runLogout(); err != nil {
			die(err)
		}
	case "doctor":
		os.Exit(runDoctor())
	case "update":
		if err := runUpdate(); err != nil {
			die(err)
		}
	case "uninstall":
		runUninstall()
	case "version", "--version", "-v":
		fmt.Println("ks", version)
	case "help", "--help", "-h":
		usage()
	default:
		cr, ok := hostedToken()
		if !ok {
			fmt.Fprintln(os.Stderr, "Not signed in. Run: ks login")
			os.Exit(2)
		}
		if !runHosted(cr, verb) {
			fmt.Fprintf(os.Stderr, "unknown verb %q\n\n", verb)
			usage()
			os.Exit(2)
		}
	}
}

func usage() {
	fmt.Print(`ks — durable agent sessions on KeepState

usage:
  ks login [--ctl URL]        sign in (browser device flow)
  ks run [--image I] [--budget N]   start a hosted session
  ks checkpoint <id> [--stop] save the session (durably); --stop parks it
  ks wake <id>                resume a parked session (mid-sentence)
  ks kill <id> [--force]      destroy a session (guarded: checkpoint first)
  ks meter <id> [--json]      spend, budget, key sources
  ks exec <id> <cmd...>       run one command in the session
  ks doctor                   connectivity, token, version
  ks update                   self-update (checksum-verified)
  ks logout                   remove the stored token
  ks uninstall                how to remove ks completely
  ks version                  print the version

Sessions survive kills: checkpoint, wake, and the agent resumes exactly
where it stopped — files, memory, and running processes intact.
`)
}

func die(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}

func arg(i int, usageLine string) string {
	if len(os.Args) <= i {
		die(fmt.Errorf("usage: %s", usageLine))
	}
	return os.Args[i]
}

func flagValue(name, def string) string {
	for i, a := range os.Args {
		if a == name && i+1 < len(os.Args) {
			return os.Args[i+1]
		}
	}
	return def
}

func hasFlag(name string) bool {
	for _, a := range os.Args {
		if a == name {
			return true
		}
	}
	return false
}
