package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const cursorCheckout = "c0ffee00-1111-4222-8333-444455556666"

func copyCursorFixture(t *testing.T, dir string, wal bool) string {
	t.Helper()
	src := "testdata/import/cursor"
	if wal {
		src += "/wal"
	}
	for _, n := range []string{"state.vscdb", "state.vscdb-wal"} {
		b, err := os.ReadFile(filepath.Join(src, n))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, n), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, "state.vscdb")
}

func TestCursorStoreReadsChatsAndSkipsEmptyAndOrphans(t *testing.T) {
	store := copyCursorFixture(t, t.TempDir(), false)
	cs, err := openCursorStore(store)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	chats, err := cs.chats()
	if err != nil {
		t.Fatal(err)
	}
	if len(chats) != 2 {
		t.Fatalf("an empty chat (only an orphaned header) must not be listed: %+v", chats)
	}
	if chats[0].ID != cursorCheckout || chats[0].Name != "Checkout endpoint" || chats[0].Model != "claude-opus-4-8" || chats[0].Folder != "/Users/alice/projects/shop" || chats[0].Bubbles != 4 {
		t.Fatalf("the newest chat: %+v", chats[0])
	}
	turns, err := cs.turns(chats[0])
	if err != nil || len(turns) != 4 || turns[0].Role != "user" || turns[1].Role != "assistant" {
		t.Fatalf("turns: %v %+v", err, turns)
	}
	if !strings.Contains(turns[0].Text, "FACT-CHARLIE-9902") {
		t.Fatalf("the first bubble's text: %q", turns[0].Text)
	}
}

func TestCursorStoreReadsTheWriteAheadLog(t *testing.T) {
	store := copyCursorFixture(t, t.TempDir(), true)
	cs, err := openCursorStore(store)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	chats, err := cs.chats()
	if err != nil || len(chats) == 0 || chats[0].ID != cursorCheckout {
		t.Fatalf("with its log, the store holds the checkout chat first: %v %+v", err, chats)
	}
	// without the log the file holds only the older chat: the reader needs the log
	only := t.TempDir()
	b, _ := os.ReadFile("testdata/import/cursor/wal/state.vscdb")
	_ = os.WriteFile(filepath.Join(only, "state.vscdb"), b, 0o644)
	cs2, err := openCursorStore(filepath.Join(only, "state.vscdb"))
	if err != nil {
		t.Fatal(err)
	}
	defer cs2.Close()
	old, _ := cs2.chats()
	for _, c := range old {
		if c.ID == cursorCheckout {
			t.Fatalf("the checkout chat is only in the log; the file alone must not hold it")
		}
	}
}

func TestCursorStoreIsCopiedAndTheOriginalIsUntouched(t *testing.T) {
	dir := t.TempDir()
	store := copyCursorFixture(t, dir, true)
	past := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	for _, n := range []string{"state.vscdb", "state.vscdb-wal"} {
		_ = os.Chtimes(filepath.Join(dir, n), past, past)
	}
	before := map[string][]byte{}
	for _, n := range []string{"state.vscdb", "state.vscdb-wal"} {
		before[n], _ = os.ReadFile(filepath.Join(dir, n))
	}
	cs, err := openCursorStore(store)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(cs.dir, dir) || cs.db.f.Name() == store {
		t.Fatalf("the original was opened (%s); a private copy must be read", cs.db.f.Name())
	}
	if _, err := cs.chats(); err != nil {
		t.Fatal(err)
	}
	copyDir := cs.dir
	cs.Close()
	if _, err := os.Stat(copyDir); !os.IsNotExist(err) {
		t.Fatalf("the copy was not removed: %s", copyDir)
	}
	for n, b := range before {
		got, _ := os.ReadFile(filepath.Join(dir, n))
		fi, _ := os.Stat(filepath.Join(dir, n))
		if !bytes.Equal(got, b) || !fi.ModTime().Equal(past) {
			t.Fatalf("%s was changed (mtime %v)", n, fi.ModTime())
		}
	}
	// no sidecar file appeared beside the original
	ents, _ := os.ReadDir(dir)
	if len(ents) != 2 {
		t.Fatalf("files appeared beside the original: %v", ents)
	}
}

func TestCursorStoreOfAnUnknownShapeIsSaidPlainly(t *testing.T) {
	dir := t.TempDir()
	notdb := filepath.Join(dir, "state.vscdb")
	_ = os.WriteFile(notdb, []byte("this is not a database"), 0o644)
	if _, err := openCursorStore(notdb); err == nil || !strings.Contains(err.Error(), "shape this client does not know") || !strings.Contains(err.Error(), "nothing was imported") {
		t.Fatalf("a file that is not SQLite: %v", err)
	}
	// a real database with no cursorDiskKV table: the older chat store of the fixture has one,
	// so cut the table name out of a copy of the schema by reading ItemTable-only bytes
	b, _ := os.ReadFile("testdata/import/cursor/state.vscdb")
	b = bytes.ReplaceAll(b, []byte("cursorDiskKV"), []byte("cursorDiskXV"))
	alt := filepath.Join(dir, "other.vscdb")
	_ = os.WriteFile(alt, b, 0o644)
	if _, err := openCursorStore(alt); err == nil || !strings.Contains(err.Error(), "shape this client does not know") {
		t.Fatalf("a store without the chat table: %v", err)
	}
	if _, err := openCursorStore(filepath.Join(dir, "missing.vscdb")); err == nil || !strings.Contains(err.Error(), "--store") {
		t.Fatalf("a missing store: %v", err)
	}
}
