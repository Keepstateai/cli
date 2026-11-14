// import_cursor.go: reading Cursor's local chat store, read-only.
//
// Cursor keeps its chats in a SQLite file in its own user data folder. Its
// format is not documented by Cursor and changes between versions, so this is
// best effort and says so. Cursor may be running and hold the file, so the
// store and its log files are copied to a private folder first and only the
// copy is read; the original is never opened for writing and never changed.
// Nothing is resumed in Cursor's terms: what comes out is the text of a chat,
// which goes across as a written summary.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// cursorMaxCopy is the largest store this client will copy: Cursor's file can
// grow to tens of gigabytes, and a copy that size would fill the disk.
const cursorMaxCopy = int64(8) << 30

func cursorDefaultStore() string {
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Cursor", "User", "globalStorage", "state.vscdb")
	case "windows":
		return filepath.Join(os.Getenv("APPDATA"), "Cursor", "User", "globalStorage", "state.vscdb")
	}
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "Cursor", "User", "globalStorage", "state.vscdb")
	}
	return filepath.Join(home, ".config", "Cursor", "User", "globalStorage", "state.vscdb")
}

type cursorChat struct {
	ID        string
	Name      string
	CreatedAt int64
	UpdatedAt int64
	Model     string
	Folder    string
	Heads     []string // bubble ids in order
	present   map[string]bool
	Bubbles   int // headers whose bubble exists
}

type cursorTurn struct{ Role, Text string }

// cursorTextFilter, set only by tests, changes each bubble's text as it is
// read (the synthetic store keeps secret markers that a test expands).
var cursorTextFilter func(string) string

type cursorStore struct {
	db      *sqliteDB
	dir     string
	root    int
	Orphans int
}

var errCursorShape = fmt.Errorf("this build of Cursor stores its chats in a shape this client does not know, so nothing was imported; Cursor's storage is not documented and this reading is best effort")

// openCursorStore copies the store (and its log files) to a private folder
// and opens the copy. Close removes the copy.
func openCursorStore(path string) (*cursorStore, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("Cursor's chat store was not found at %s (%v); name it with --store", path, err)
	}
	if fi.Size() > cursorMaxCopy {
		return nil, fmt.Errorf("Cursor's chat store at %s is %s, too large to copy safely; nothing was read", path, bytesText(fi.Size()))
	}
	dir, err := os.MkdirTemp("", "ks-cursor-")
	if err != nil {
		return nil, err
	}
	copyTo := filepath.Join(dir, "state.vscdb")
	for _, ext := range []string{"", "-wal", "-shm"} {
		if err := copyFileReadOnly(path+ext, copyTo+ext, ext == ""); err != nil {
			os.RemoveAll(dir)
			return nil, err
		}
	}
	db, err := openSQLite(copyTo)
	if err != nil {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("%v: %w", err, errCursorShape)
	}
	root, ok, err := db.tableRoot("cursorDiskKV")
	if err != nil || !ok {
		db.Close()
		os.RemoveAll(dir)
		return nil, errCursorShape
	}
	return &cursorStore{db: db, dir: dir, root: root}, nil
}

func (c *cursorStore) Close() {
	c.db.Close()
	os.RemoveAll(c.dir)
}

// copyFileReadOnly copies src to dst; a missing optional file is not an error.
func copyFileReadOnly(src, dst string, required bool) error {
	in, err := os.Open(src)
	if err != nil {
		if os.IsNotExist(err) && !required {
			return nil
		}
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func jsonInt(v any) int64 {
	switch x := v.(type) {
	case float64:
		return int64(x)
	case json.Number:
		n, _ := x.Int64()
		return n
	}
	return 0
}

// chats lists the chats that hold at least one bubble, newest first.
func (c *cursorStore) chats() ([]cursorChat, error) {
	byID := map[string]*cursorChat{}
	present := map[string]map[string]bool{}
	composers := 0
	err := c.db.scanTable(c.root, func(_ int64, cols []sqliteValue) error {
		if len(cols) < 2 {
			return nil
		}
		key := cols[0].String()
		switch {
		case strings.HasPrefix(key, "composerData:"):
			var d map[string]any
			if json.Unmarshal(cols[1].Bytes, &d) != nil {
				return nil
			}
			composers++
			ch := &cursorChat{ID: strings.TrimPrefix(key, "composerData:"), Name: asStr(d["name"]), CreatedAt: jsonInt(d["createdAt"]), UpdatedAt: jsonInt(d["lastUpdatedAt"])}
			if m := asMap(d["modelConfig"]); m != nil {
				ch.Model = asStr(m["modelName"])
			}
			if ctx := asMap(d["context"]); ctx != nil {
				for _, fs := range asList(ctx["folderSelections"]) {
					if u := asStr(asMap(fs)["uri"]); u != "" && ch.Folder == "" {
						if pu, err := url.Parse(u); err == nil && pu.Scheme == "file" {
							ch.Folder = pu.Path
						}
					}
				}
			}
			for _, h := range asList(d["fullConversationHeadersOnly"]) {
				if id := asStr(asMap(h)["bubbleId"]); id != "" {
					ch.Heads = append(ch.Heads, id)
				}
			}
			byID[ch.ID] = ch
		case strings.HasPrefix(key, "bubbleId:"):
			parts := strings.SplitN(strings.TrimPrefix(key, "bubbleId:"), ":", 2)
			if len(parts) == 2 {
				if present[parts[0]] == nil {
					present[parts[0]] = map[string]bool{}
				}
				present[parts[0]][parts[1]] = true
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("%v: %w", err, errCursorShape)
	}
	if composers == 0 {
		return nil, errCursorShape
	}
	var out []cursorChat
	for _, ch := range byID {
		ch.present = present[ch.ID]
		for _, h := range ch.Heads {
			if ch.present[h] {
				ch.Bubbles++
			}
		}
		if ch.Bubbles > 0 {
			out = append(out, *ch)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].UpdatedAt, out[j].UpdatedAt
		if a == 0 {
			a = out[i].CreatedAt
		}
		if b == 0 {
			b = out[j].CreatedAt
		}
		if a != b {
			return a > b
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// turns reads one chat's text in the order its header list gives; a header
// whose bubble is missing (an orphan) is skipped and counted.
func (c *cursorStore) turns(ch cursorChat) ([]cursorTurn, error) {
	want := map[string]bool{}
	for _, h := range ch.Heads {
		want[h] = true
	}
	text := map[string]cursorTurn{}
	prefix := "bubbleId:" + ch.ID + ":"
	err := c.db.scanTable(c.root, func(_ int64, cols []sqliteValue) error {
		if len(cols) < 2 {
			return nil
		}
		key := cols[0].String()
		if !strings.HasPrefix(key, prefix) {
			return nil
		}
		id := strings.TrimPrefix(key, prefix)
		if !want[id] {
			return nil
		}
		var b map[string]any
		if json.Unmarshal(cols[1].Bytes, &b) != nil {
			return nil
		}
		role := "assistant"
		if jsonInt(b["type"]) == 1 {
			role = "user"
		}
		txt := asStr(b["text"])
		if cursorTextFilter != nil {
			txt = cursorTextFilter(txt)
		}
		if t := strings.TrimSpace(txt); t != "" {
			text[id] = cursorTurn{role, t}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("%v: %w", err, errCursorShape)
	}
	var out []cursorTurn
	for _, h := range ch.Heads {
		if t, ok := text[h]; ok {
			out = append(out, t)
		} else if !ch.present[h] {
			c.Orphans++
		}
	}
	return out, nil
}
