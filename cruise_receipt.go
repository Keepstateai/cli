// cruise_receipt.go: ks cruise receipt JOB [--out FILE] downloads the signed
// receipt of an accepted Cruise job, exactly as the service sealed it.
//
// The status of an accepted job names the receipt file in
// result.receipt (its sha256, whether it carries an Ed25519 signature anyone
// can check with the published public key, and where to fetch it). This verb
// reads the status, downloads GET /api/jobs/{id}/receipt, and checks the
// bytes BEFORE anything is written: the X-KS-Sha256 header and the sha256 of
// the bytes received must both equal the digest the status names.
//
// BYTES IN, BYTES OUT. The receipt is never parsed here and never written
// out again: the file written is the byte slice that was received. The
// signature covers the receipt's canonical form and the digest covers its
// bytes; a parsed and re-written copy changes numbers and escapes, and the
// public verifier then refuses it. cruise_receipt_test.go holds that line.
//
// The file is linked into place, which never replaces an existing file.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// liveReceipt is result.receipt of the job status: null when the result was
// accepted before the service kept receipt files.
type liveReceipt struct {
	SHA256             string `json:"sha256"`
	SigAlg             string `json:"sig_alg"`
	KeyID              string `json:"key_id"`
	PubliclyVerifiable bool   `json:"publicly_verifiable"`
	Download           string `json:"download"`
}

// receiptVerifiabilityWords says, in plain words, who can check the receipt.
func receiptVerifiabilityWords(r *liveReceipt) string {
	if r.PubliclyVerifiable {
		return "publicly verifiable: anyone can check its Ed25519 signature with the published public key, without an account"
	}
	return "service-verifiable only: it carries the service's own signature, which only KeepState can check; it has no public signature"
}

// fetchJobReceipt downloads the receipt and checks it against the digest the
// status names, before anything else reads it.
func fetchJobReceipt(c hostedCreds, id, want string) ([]byte, error) {
	resp, err := hostedDo(c, "GET", inContext(c, "/api/jobs/"+id+"/receipt"), "", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, fmt.Errorf("the receipt download was interrupted: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return nil, hostedError("GET", "/api/jobs/"+id+"/receipt", resp, raw)
	}
	header := bareHex(resp.Header.Get("X-KS-Sha256"))
	sum := sha256.Sum256(raw)
	got := hex.EncodeToString(sum[:])
	switch {
	case header == "":
		return nil, integrity("receipt_unsigned", "the receipt arrived without its X-KS-Sha256; nothing was written")
	case header != want:
		return nil, integrity("receipt_mismatch", fmt.Sprintf("the receipt served names sha256 %s, and the job's result names %s; nothing was written", short(header), short(want)))
	case got != want:
		return nil, integrity("receipt_mismatch", fmt.Sprintf("the receipt received hashes to %s, not the %s the job's result names; nothing was written", short(got), short(want)))
	}
	return raw, nil
}

// writeNewFile writes data to dest without ever replacing a file.
func writeNewFile(dest string, data []byte) error {
	if _, err := os.Lstat(dest); err == nil {
		return &cliError{Code: exitConflict, Kind: "file_exists", Message: dest + " exists and is never written over; nothing was written", NextAction: "choose another name with --out"}
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".ks-receipt-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Link(tmp.Name(), dest); err != nil {
		if errors.Is(err, os.ErrExist) {
			return &cliError{Code: exitConflict, Kind: "file_exists", Message: dest + " appeared during the download and is never written over; nothing was written", NextAction: "choose another name with --out"}
		}
		return err
	}
	return nil
}

// cruiseReceipt is ks cruise receipt JOB [--out FILE].
func cruiseReceipt(inv *Invocation) {
	c := mustCreds()
	id := strings.TrimSpace(inv.Arg(0))
	dest := id + ".receipt.json"
	if inv.Set("out") {
		dest = inv.Str("out")
	}
	st, err := fetchJobStatus(c, id)
	if err != nil {
		fail(err)
		return
	}
	if st.Result == nil || st.Result.Receipt == nil || !sha256Hex64.MatchString(bareHex(st.Result.Receipt.SHA256)) {
		fail(&cliError{Code: exitFailed, Kind: "no_receipt",
			Message:    fmt.Sprintf("job %s has no signed receipt file: it is not accepted, or it was accepted before the service kept them; nothing was written", id),
			NextAction: "ks cruise result " + id + " (the result and its receipt signature as recorded)"})
		return
	}
	rc := st.Result.Receipt
	raw, err := fetchJobReceipt(c, id, bareHex(rc.SHA256))
	if err != nil {
		fail(err)
		return
	}
	if err := writeNewFile(dest, raw); err != nil {
		fail(err)
		return
	}
	emit(map[string]any{"job_id": id, "path": dest, "bytes": len(raw), "sha256": bareHex(rc.SHA256), "sig_alg": rc.SigAlg, "key_id": rc.KeyID,
		"publicly_verifiable": rc.PubliclyVerifiable}, func() {
		progress("receipt %s: %s bytes, sha256 %s matches the job's record", id, commas(int64(len(raw))), short(bareHex(rc.SHA256)))
		progress("this receipt is %s", receiptVerifiabilityWords(rc))
		fmt.Println(dest)
	})
}
