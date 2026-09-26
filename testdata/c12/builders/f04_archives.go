package c12

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"math/rand"
	"strings"
	"time"
)

// ArchiveCase is one F04 archive with what each side must do with it.
//
// Server is the control plane's contract (KS-027 server half): accept, or
// refuse with a reason containing ServerReason. Client is the client's
// contract for the same bytes when it downloads or applies a result; the
// client half is recorded for the client repository, not asserted here.
type ArchiveCase struct {
	Name         string
	Bytes        []byte
	Manifest     map[string]any // nil: no manifest binding
	ServerAccept bool
	ServerReason string
	Client       string
}

type entry struct {
	name     string
	typ      byte
	body     []byte
	size     int64 // declared size when it differs from len(body); -1: len(body)
	linkname string
}

var epoch = time.Unix(0, 0).UTC()

// tgz writes entries exactly as given -- including the ones a careful
// archiver would never write -- with every variable header field fixed.
func tgz(entries []entry, truncateTo int) []byte {
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	for _, e := range entries {
		size := int64(len(e.body))
		if e.size >= 0 && e.typ == tar.TypeReg {
			size = e.size
		}
		if e.typ != tar.TypeReg {
			size = 0
		}
		h := &tar.Header{Name: e.name, Typeflag: e.typ, Size: size, Mode: 0o644, ModTime: epoch,
			Linkname: e.linkname, Format: tar.FormatPAX}
		if e.typ == tar.TypeDir {
			h.Mode = 0o755
		}
		if err := tw.WriteHeader(h); err != nil {
			panic("c12: " + err.Error())
		}
		if e.typ == tar.TypeReg {
			body := e.body
			if int64(len(body)) > size {
				body = body[:size]
			}
			if _, err := tw.Write(body); err != nil && size >= int64(len(e.body)) {
				panic("c12: " + err.Error())
			}
		}
	}
	// a short body leaves the writer mid-entry; Close would refuse, so the
	// stream is flushed as far as it goes -- that is the attack
	_ = tw.Flush()
	_ = tw.Close()
	var out bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&out, gzip.BestCompression)
	zw.ModTime = epoch
	zw.Name = ""
	_, _ = zw.Write(raw.Bytes())
	_ = zw.Close()
	b := out.Bytes()
	if truncateTo > 0 && truncateTo < len(b) {
		b = b[:truncateTo]
	}
	return b
}

func reg(name string, body []byte) entry {
	return entry{name: name, typ: tar.TypeReg, body: body, size: -1}
}

// F04ArchiveCases returns every F04 archive, deterministically for a seed.
func F04ArchiveCases(seed int64) []ArchiveCase {
	r := rand.New(rand.NewSource(seed))
	app := append([]byte("print('f04 fixture')\n# "), filler(r, 64)...)
	readme := append([]byte("# f04 fixture\n"), filler(r, 64)...)
	clean := []entry{reg("README.md", readme), reg("src/app.py", app)}
	with := func(extra ...entry) []entry { return append(append([]entry{}, clean...), extra...) }
	bomb := bytes.Repeat([]byte{0}, 51<<20) // 51 MiB of zeros: tiny compressed, over the 50 MiB bound

	cleanBytes := tgz(clean, 0)
	cases := []ArchiveCase{
		{Name: "clean", Bytes: cleanBytes, ServerAccept: true,
			Client: "applies to a clean base; the downloaded bytes' sha256 equals the recorded one"},
		{Name: "traversal-dotdot", Bytes: tgz(with(reg("../escape.txt", []byte("x"))), 0), ServerReason: "escapes the workspace",
			Client: "refuse before writing anything"},
		{Name: "traversal-inner", Bytes: tgz(with(reg("src/../../escape.txt", []byte("x"))), 0), ServerReason: "escapes the workspace",
			Client: "refuse before writing anything"},
		{Name: "absolute-path", Bytes: tgz(with(reg("/etc/f04-absolute", []byte("x"))), 0), ServerReason: "is an absolute path",
			Client: "refuse before writing anything"},
		{Name: "backslash-path", Bytes: tgz(with(reg(`src\..\..\escape.txt`, []byte("x"))), 0), ServerReason: "backslash",
			Client: "refuse; never reinterpret a backslash as a separator"},
		{Name: "duplicate-name", Bytes: tgz(with(reg("src/app.py", []byte("second copy"))), 0), ServerReason: "appears twice",
			Client: "refuse; the second copy must not silently win"},
		{Name: "symlink-entry", Bytes: tgz(with(entry{name: "link", typ: tar.TypeSymlink, linkname: "/etc/passwd", size: -1}), 0), ServerReason: "is a link entry",
			Client: "refuse; never create a link from a result"},
		{Name: "hardlink-entry", Bytes: tgz(with(entry{name: "hard", typ: tar.TypeLink, linkname: "README.md", size: -1}), 0), ServerReason: "is a link entry",
			Client: "refuse; never create a link from a result"},
		{Name: "directory-entry", Bytes: tgz(with(entry{name: "src/sub", typ: tar.TypeDir, size: -1}), 0), ServerReason: "is a directory entry",
			Client: "directories are implied by file paths; an explicit one is refused"},
		{Name: "fifo-entry", Bytes: tgz(with(entry{name: "pipe", typ: tar.TypeFifo, size: -1}), 0), ServerReason: "is not a regular file",
			Client: "refuse special files"},
		{Name: "device-entry", Bytes: tgz(with(entry{name: "dev", typ: tar.TypeChar, size: -1}), 0), ServerReason: "is not a regular file",
			Client: "refuse special files"},
		{Name: "excluded-git", Bytes: tgz(with(reg(".git/config", []byte("[core]\n"))), 0), ServerReason: "excludes",
			Client: "never write under .git or .keepstate"},
		{Name: "unclean-path", Bytes: tgz(with(reg("./src/dot.py", []byte("x"))), 0), ServerReason: "not a clean relative path",
			Client: "refuse"},
		{Name: "control-char-name", Bytes: tgz(with(reg("src/a\nb.py", []byte("x"))), 0), ServerReason: "control character",
			Client: "refuse; a name must not be able to forge terminal output"},
		{Name: "long-path", Bytes: tgz(with(reg(strings.Repeat("d/", 520)+"f.txt", []byte("x"))), 0), ServerReason: "longer than",
			Client: "refuse"},
		{Name: "huge-expansion", Bytes: tgz(with(reg("big.bin", bomb)), 0), ServerReason: "expands beyond",
			Client: "stop at the bound while streaming; never expand the whole archive first"},
		{Name: "header-longer-than-body", Bytes: tgz(with(entry{name: "short.txt", typ: tar.TypeReg, body: []byte("only ten b"), size: 4096}), 0),
			ServerReason: "", Client: "refuse a partial entry; nothing of it is written"},
		{Name: "truncated-stream", Bytes: tgz(clean, len(cleanBytes)/2), ServerReason: "",
			Client: "a partial download: the sha256 does not match; nothing is applied and the download can resume or restart"},
		{Name: "not-gzip", Bytes: []byte("PK\x03\x04 this is not a gzip stream"), ServerReason: "not a gzip stream",
			Client: "refuse"},
		{Name: "digest-mismatch", Bytes: cleanBytes, Manifest: map[string]any{"initial_state_digest": strings.Repeat("0", 64)},
			ServerReason: "tree digest", Client: "the result's recorded hash does not match: refuse to apply"},
		// Accepted by the server, whose guest filesystem is case- and
		// normalization-sensitive; dangerous to APPLY on a case-insensitive
		// or normalizing local filesystem. The client half decides.
		{Name: "case-collision", Bytes: tgz(with(reg("readme.md", []byte("lower"))), 0), ServerAccept: true,
			Client: "on a case-insensitive filesystem README.md and readme.md collide: refuse to apply, naming both"},
		{Name: "unicode-normalization-pair", Bytes: tgz(with(reg("café.txt", []byte("nfc")), reg("café.txt", []byte("nfd"))), 0), ServerAccept: true,
			Client: "on a normalizing filesystem the NFC and NFD names collide: refuse to apply, naming both"},
	}
	return cases
}

// F04ApplyScenarios are the result-application attacks that exist only on
// the client's side of a download: they are not archive bytes, so the
// server has nothing to validate. Recorded for the client half.
var F04ApplyScenarios = []struct{ Name, Setup, Expect string }{
	{"partial-download", "the connection drops after half of the result's bytes", "the sha256 check fails; nothing is applied; resume or restart"},
	{"hash-mismatch", "the bytes are complete but differ from the recorded sha256", "refuse to apply; report both digests"},
	{"pre-existing-target-race", "a file appears at a target path between the diff preview and the apply", "refuse that path; never overwrite a file the preview did not show"},
	{"dirty-local-base", "the local file was edited after the base the result was computed from", "refuse the conflicting path; the local edit survives"},
}
