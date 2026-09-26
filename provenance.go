// provenance.go: verifying that a ks binary was built by this repository's
// release workflow (KS-006), through GitHub's build-provenance
// attestations, by asking the `gh` command line tool:
//
//	gh attestation verify <binary> --repo Keepstateai/cli --format json
//
// The client keeps no module dependencies, so it verifies nothing of the
// sigstore bundle itself; gh does. What this file does is refuse to call
// anything verified that gh did not verify: gh missing, gh failing, gh
// timing out, gh printing something that is not a verification result --
// each reads "NOT verified" with the reason. Nothing passes by default.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const provenanceRepo = "Keepstateai/cli"

type provenanceResult struct {
	Verified bool   `json:"verified"`
	Refused  bool   `json:"refused"` // gh ran and said the binary does NOT verify
	Reason   string `json:"reason"`
	Repo     string `json:"repo"`
	Binary   string `json:"binary"`
}

func verifyProvenance(binary string) provenanceResult {
	r := provenanceResult{Repo: provenanceRepo, Binary: binary}
	gh, err := exec.LookPath("gh")
	if err != nil {
		r.Reason = "the gh command line tool is not installed, so the build provenance attestation could not be checked"
		return r
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, gh, "attestation", "verify", binary, "--repo", provenanceRepo, "--format", "json")
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	runErr := cmd.Run()
	if ctx.Err() != nil {
		r.Reason = "gh did not answer within 90 s"
		return r
	}
	if runErr != nil {
		var ee *exec.ExitError
		msg := strings.TrimSpace(se.String())
		if len(msg) > 300 {
			msg = msg[:300]
		}
		if errors.As(runErr, &ee) {
			// gh ran and did not verify: a mismatch, a missing attestation, or
			// gh's own failure (not signed in, offline) -- none is a pass
			r.Refused = strings.Contains(strings.ToLower(msg), "verif")
			r.Reason = fmt.Sprintf("gh attestation verify exited %d: %s", ee.ExitCode(), sanitize(msg))
			return r
		}
		r.Reason = "gh could not be run: " + sanitize(runErr.Error())
		return r
	}
	// exit 0 is necessary, not sufficient: the answer must be verification
	// results naming this repository's workflow
	var results []struct {
		VerificationResult *struct {
			Statement *struct {
				PredicateType string `json:"predicateType"`
			} `json:"statement"`
		} `json:"verificationResult"`
	}
	if err := json.Unmarshal(so.Bytes(), &results); err != nil || len(results) == 0 {
		r.Reason = "gh exited 0 but its answer is not a list of verification results, so nothing is claimed"
		return r
	}
	for _, x := range results {
		if x.VerificationResult == nil || x.VerificationResult.Statement == nil || x.VerificationResult.Statement.PredicateType == "" {
			r.Reason = "gh exited 0 but a result carries no verified statement, so nothing is claimed"
			return r
		}
	}
	r.Verified = true
	r.Reason = fmt.Sprintf("gh verified %d build provenance attestation(s) from %s", len(results), provenanceRepo)
	return r
}

func provenanceLine(r provenanceResult) string {
	if r.Verified {
		return "provenance: verified -- " + r.Reason
	}
	return "provenance: NOT verified -- " + r.Reason
}

func runVersion(inv *Invocation) {
	if !inv.Bool("verify") {
		emit(map[string]any{"version": version}, func() { fmt.Println("ks", version) })
		return
	}
	exe, err := os.Executable()
	if err == nil {
		exe, _ = filepath.EvalSymlinks(exe)
	}
	r := verifyProvenance(exe)
	emit(map[string]any{"version": version, "provenance": r}, func() {
		fmt.Println("ks", version)
		fmt.Println(provenanceLine(r))
	})
	switch {
	case r.Verified:
	case r.Refused:
		os.Exit(exitIntegrity)
	default:
		os.Exit(exitFailed)
	}
}
