// import_sqlite.go: a small reader of the SQLite file format, for one job:
// reading the rows of a table out of a copy of Cursor's chat store.
//
// This client takes no SQLite library. The format is public and the part
// needed is small: the file header, table b-tree pages (interior and leaf),
// records, overflow chains, and a write-ahead log, whose committed frames
// replace the pages of the file. Nothing here writes; it reads a file it was
// given (a private copy of the store, made by the caller).
//
// Only what the job needs is understood: UTF-8 databases, rowid tables. A
// file that is anything else is refused in plain words, never guessed at.
package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
)

type sqliteValue struct {
	Kind  byte // 'n' null, 'i' integer, 'f' float, 't' text, 'b' blob
	Int   int64
	Float float64
	Bytes []byte
}

func (v sqliteValue) String() string {
	switch v.Kind {
	case 'i':
		return fmt.Sprint(v.Int)
	case 'f':
		return fmt.Sprint(v.Float)
	case 't', 'b':
		return string(v.Bytes)
	}
	return ""
}

type sqliteDB struct {
	f        *os.File
	pageSize int
	usable   int
	pages    int // pages in the database, after the log
	wal      map[int][]byte
}

var errSQLite = errors.New("this file is not a SQLite database this client can read")

func openSQLite(path string) (*sqliteDB, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	hdr := make([]byte, 100)
	if _, err := f.ReadAt(hdr, 0); err != nil || string(hdr[:16]) != "SQLite format 3\x00" {
		f.Close()
		return nil, errSQLite
	}
	ps := int(binary.BigEndian.Uint16(hdr[16:18]))
	if ps == 1 {
		ps = 65536
	}
	if ps < 512 || ps&(ps-1) != 0 {
		f.Close()
		return nil, errSQLite
	}
	if enc := binary.BigEndian.Uint32(hdr[56:60]); enc != 1 && enc != 0 {
		f.Close()
		return nil, fmt.Errorf("%w (its text is not UTF-8)", errSQLite)
	}
	db := &sqliteDB{f: f, pageSize: ps, usable: ps - int(hdr[20]), wal: map[int][]byte{}}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	db.pages = int(fi.Size() / int64(ps))
	if n := int(binary.BigEndian.Uint32(hdr[28:32])); n > 0 && n <= db.pages {
		db.pages = n
	}
	if err := db.readWAL(path + "-wal"); err != nil {
		f.Close()
		return nil, err
	}
	return db, nil
}

func (db *sqliteDB) Close() { db.f.Close() }

// walChecksum is the log's checksum over 8-byte units, seeded.
func walChecksum(data []byte, bigEndian bool, s0, s1 uint32) (uint32, uint32) {
	for i := 0; i+8 <= len(data); i += 8 {
		var a, b uint32
		if bigEndian {
			a, b = binary.BigEndian.Uint32(data[i:]), binary.BigEndian.Uint32(data[i+4:])
		} else {
			a, b = binary.LittleEndian.Uint32(data[i:]), binary.LittleEndian.Uint32(data[i+4:])
		}
		s0 += a + s1
		s1 += b + s0
	}
	return s0, s1
}

// readWAL reads the committed frames of the log, if there is one: the newest
// committed copy of each page replaces the file's.
func (db *sqliteDB) readWAL(path string) error {
	b, err := os.ReadFile(path)
	if err != nil || len(b) < 32 {
		return nil // no log, or an empty one
	}
	magic := binary.BigEndian.Uint32(b[0:4])
	if magic != 0x377f0682 && magic != 0x377f0683 {
		return nil
	}
	big := magic&1 == 1
	if int(binary.BigEndian.Uint32(b[8:12])) != db.pageSize {
		return nil // a log of another page size is not this file's
	}
	salt1, salt2 := binary.BigEndian.Uint32(b[16:20]), binary.BigEndian.Uint32(b[20:24])
	s0, s1 := walChecksum(b[:24], big, 0, 0)
	if s0 != binary.BigEndian.Uint32(b[24:28]) || s1 != binary.BigEndian.Uint32(b[28:32]) {
		return nil
	}
	frame := 24 + db.pageSize
	committed := map[int][]byte{}
	pending := map[int][]byte{}
	for off := 32; off+frame <= len(b); off += frame {
		fh := b[off : off+24]
		if binary.BigEndian.Uint32(fh[8:12]) != salt1 || binary.BigEndian.Uint32(fh[12:16]) != salt2 {
			break
		}
		s0, s1 = walChecksum(fh[:8], big, s0, s1)
		s0, s1 = walChecksum(b[off+24:off+frame], big, s0, s1)
		if s0 != binary.BigEndian.Uint32(fh[16:20]) || s1 != binary.BigEndian.Uint32(fh[20:24]) {
			break
		}
		pg := int(binary.BigEndian.Uint32(fh[0:4]))
		if pg < 1 {
			break
		}
		page := make([]byte, db.pageSize)
		copy(page, b[off+24:off+frame])
		pending[pg] = page
		if size := int(binary.BigEndian.Uint32(fh[4:8])); size != 0 {
			for k, v := range pending {
				committed[k] = v
			}
			pending = map[int][]byte{}
			db.pages = size
		}
	}
	db.wal = committed
	return nil
}

func (db *sqliteDB) page(n int) ([]byte, error) {
	if n < 1 || n > db.pages {
		return nil, fmt.Errorf("%w (page %d is outside the file)", errSQLite, n)
	}
	if p, ok := db.wal[n]; ok {
		return p, nil
	}
	p := make([]byte, db.pageSize)
	if _, err := db.f.ReadAt(p, int64(n-1)*int64(db.pageSize)); err != nil {
		return nil, fmt.Errorf("%w (page %d could not be read)", errSQLite, n)
	}
	return p, nil
}

func varint(b []byte) (int64, int) {
	var v uint64
	for i := 0; i < 9 && i < len(b); i++ {
		if i == 8 {
			return int64(v<<8 | uint64(b[i])), 9
		}
		v = v<<7 | uint64(b[i]&0x7f)
		if b[i]&0x80 == 0 {
			return int64(v), i + 1
		}
	}
	return 0, 0
}

// payload assembles a leaf cell's payload with its overflow chain.
func (db *sqliteDB) payload(page []byte, at int, total int) ([]byte, error) {
	u := db.usable
	x := u - 35
	if total <= x {
		if at+total > len(page) {
			return nil, errSQLite
		}
		return page[at : at+total], nil
	}
	m := (u-12)*32/255 - 23
	k := m + (total-m)%(u-4)
	local := m
	if k <= x {
		local = k
	}
	if at+local+4 > len(page) {
		return nil, errSQLite
	}
	out := make([]byte, 0, total)
	out = append(out, page[at:at+local]...)
	next := int(binary.BigEndian.Uint32(page[at+local : at+local+4]))
	for next != 0 && len(out) < total {
		op, err := db.page(next)
		if err != nil {
			return nil, err
		}
		take := total - len(out)
		if take > u-4 {
			take = u - 4
		}
		out = append(out, op[4:4+take]...)
		next = int(binary.BigEndian.Uint32(op[0:4]))
	}
	if len(out) != total {
		return nil, fmt.Errorf("%w (an overflow chain ends early)", errSQLite)
	}
	return out, nil
}

// scanTable calls f with the decoded record of every row of the table b-tree
// rooted at page root, in rowid order.
func (db *sqliteDB) scanTable(root int, f func(rowid int64, cols []sqliteValue) error) error {
	return db.walkTable(root, 0, f)
}

func (db *sqliteDB) walkTable(n, depth int, f func(int64, []sqliteValue) error) error {
	if depth > 40 {
		return fmt.Errorf("%w (the table is nested too deep)", errSQLite)
	}
	pg, err := db.page(n)
	if err != nil {
		return err
	}
	h := 0
	if n == 1 {
		h = 100
	}
	typ := pg[h]
	cells := int(binary.BigEndian.Uint16(pg[h+3 : h+5]))
	switch typ {
	case 0x0d: // table leaf
		ptr := h + 8
		for i := 0; i < cells; i++ {
			off := int(binary.BigEndian.Uint16(pg[ptr+2*i:]))
			if off >= len(pg) {
				return errSQLite
			}
			size, a := varint(pg[off:])
			rowid, b := varint(pg[off+a:])
			if a == 0 || b == 0 {
				return errSQLite
			}
			pl, err := db.payload(pg, off+a+b, int(size))
			if err != nil {
				return err
			}
			cols, err := decodeSQLiteRecord(pl)
			if err != nil {
				return err
			}
			if err := f(rowid, cols); err != nil {
				return err
			}
		}
	case 0x05: // table interior
		ptr := h + 12
		for i := 0; i < cells; i++ {
			off := int(binary.BigEndian.Uint16(pg[ptr+2*i:]))
			if off+4 > len(pg) {
				return errSQLite
			}
			if err := db.walkTable(int(binary.BigEndian.Uint32(pg[off:])), depth+1, f); err != nil {
				return err
			}
		}
		return db.walkTable(int(binary.BigEndian.Uint32(pg[h+8:h+12])), depth+1, f)
	default:
		return fmt.Errorf("%w (a page of type %d where a table page belongs)", errSQLite, typ)
	}
	return nil
}

func decodeSQLiteRecord(p []byte) ([]sqliteValue, error) {
	hl, a := varint(p)
	if a == 0 || int(hl) > len(p) {
		return nil, errSQLite
	}
	var types []int64
	for i := a; i < int(hl); {
		t, n := varint(p[i:])
		if n == 0 {
			return nil, errSQLite
		}
		types = append(types, t)
		i += n
	}
	body := p[hl:]
	pos := 0
	out := make([]sqliteValue, 0, len(types))
	need := func(n int) ([]byte, error) {
		if pos+n > len(body) {
			return nil, errSQLite
		}
		b := body[pos : pos+n]
		pos += n
		return b, nil
	}
	for _, t := range types {
		switch {
		case t == 0:
			out = append(out, sqliteValue{Kind: 'n'})
		case t >= 1 && t <= 6:
			size := map[int64]int{1: 1, 2: 2, 3: 3, 4: 4, 5: 6, 6: 8}[t]
			b, err := need(size)
			if err != nil {
				return nil, err
			}
			var v int64
			if b[0]&0x80 != 0 {
				v = -1
			}
			for _, c := range b {
				v = v<<8 | int64(c)
			}
			out = append(out, sqliteValue{Kind: 'i', Int: v})
		case t == 7:
			b, err := need(8)
			if err != nil {
				return nil, err
			}
			out = append(out, sqliteValue{Kind: 'f', Float: math.Float64frombits(binary.BigEndian.Uint64(b))})
		case t == 8:
			out = append(out, sqliteValue{Kind: 'i', Int: 0})
		case t == 9:
			out = append(out, sqliteValue{Kind: 'i', Int: 1})
		case t >= 12 && t%2 == 0:
			b, err := need(int(t-12) / 2)
			if err != nil {
				return nil, err
			}
			out = append(out, sqliteValue{Kind: 'b', Bytes: b})
		case t >= 13:
			b, err := need(int(t-13) / 2)
			if err != nil {
				return nil, err
			}
			out = append(out, sqliteValue{Kind: 't', Bytes: b})
		default:
			return nil, errSQLite
		}
	}
	return out, nil
}

// tableRoot finds a table's root page in the schema (page 1).
func (db *sqliteDB) tableRoot(name string) (int, bool, error) {
	root, found := 0, false
	err := db.scanTable(1, func(_ int64, c []sqliteValue) error {
		if len(c) >= 4 && c[0].String() == "table" && c[1].String() == name {
			root, found = int(c[3].Int), true
		}
		return nil
	})
	return root, found, err
}
