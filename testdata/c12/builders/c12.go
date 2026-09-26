// Package c12 builds the deterministic test fixtures of the specification's
// C12 table (KS-091) that are data rather than a running service: the
// hostile workspace (F03) and the archive/result attacks (F04). Every
// fixture is synthetic: no real secret, credential or customer byte exists
// in any of them, and building one needs no network, no key and no service.
//
// Determinism is the contract. The same version and seed produce the same
// bytes on every platform, so an independent reviewer can rebuild a fixture
// from its registry row (registry.json: generator, version, seed, expected)
// and reproduce a result without the author (VER-091-2). The self-tests in
// this package prove that by building twice and comparing digests.
//
// Consumers: the control plane's archive validation (ctl, server half) and
// the client's selection and result application (keepstate-cli, client
// half, through `go run ./fixtures/c12/cmd/c12gen`, which writes the same
// bytes to a directory).
package c12

import (
	"crypto/sha256"
	"encoding/hex"
	"math/rand"
)

// Version is bumped whenever any fixture's bytes change on purpose. A
// recorded result names the version it ran against.
const Version = "c12-fixtures/1"

// DefaultSeed is the seed the registry records for every generated fixture.
const DefaultSeed int64 = 91

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// filler is seeded, printable and stable: it gives fixture files distinct
// content without any value that could be mistaken for a credential.
func filler(r *rand.Rand, n int) []byte {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789 "
	b := make([]byte, n)
	for i := range b {
		b[i] = alphabet[r.Intn(len(alphabet))]
	}
	return b
}

// SHA256Hex is the digest recorded for every generated fixture file.
func SHA256Hex(b []byte) string { return sha(b) }
