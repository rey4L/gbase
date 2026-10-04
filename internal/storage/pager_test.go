package storage

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func openTest(t *testing.T, path string) *Pager {
	t.Helper()
	p, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { p.Close() })
	return p
}

func beginTest(t *testing.T, p *Pager) *Tx {
	t.Helper()
	x, e := p.Begin()
	if e != nil {
		t.Fatal(e)
	}
	return x
}

func must(t *testing.T, e error) {
	t.Helper()
	if e != nil {
		t.Fatal(e)
	}
}
func allocTest(t *testing.T, x *Tx) uint32 { t.Helper(); id, e := x.Alloc(); must(t, e); return id }
func pageByte(v byte) []byte               { return bytes.Repeat([]byte{v}, PageSize) }
func readEquals(t *testing.T, x *Tx, id uint32, v byte) {
	t.Helper()
	b, e := x.Read(id)
	must(t, e)
	expected := pageByte(v)
	seal(expected)
	if !bytes.Equal(b, expected) {
		t.Fatalf("page %d differs from %d", id, v)
	}
}

func TestTransactions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	p := openTest(t, path)
	x := beginTest(t, p)
	if x.PageCount() != 1 || x.Root() != 0 {
		t.Fatal("initial metadata")
	}
	id := allocTest(t, x)
	if id != 1 {
		t.Fatal(id)
	}
	readEquals(t, x, id, 0)
	data := pageByte(7)
	must(t, x.Write(id, data))
	data[0] = 99
	readEquals(t, x, id, 7)
	b, e := x.Read(id)
	must(t, e)
	b[0] = 42
	readEquals(t, x, id, 7)
	x.SetRoot(id)
	before, e := os.ReadFile(path)
	must(t, e)
	if len(before) != PageSize {
		t.Fatal("precommit write")
	}
	must(t, x.Commit())
	if _, e = x.Read(id); !errors.Is(e, ErrClosed) {
		t.Fatal(e)
	}
	x = beginTest(t, p)
	must(t, x.Write(id, pageByte(8)))
	allocTest(t, x)
	x.SetRoot(0)
	must(t, x.Rollback())
	x = beginTest(t, p)
	readEquals(t, x, id, 7)
	if x.PageCount() != 2 || x.Root() != id {
		t.Fatal("rollback metadata")
	}
	must(t, x.Rollback())
	must(t, p.Close())
	p = openTest(t, path)
	x = beginTest(t, p)
	readEquals(t, x, id, 7)
	if x.Root() != id {
		t.Fatal("root not persisted")
	}
	if _, e = os.Stat(path + "-journal"); !errors.Is(e, os.ErrNotExist) {
		t.Fatal(e)
	}
}

func TestFreeAndSavepoints(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	p := openTest(t, path)
	x := beginTest(t, p)
	a, b, c := allocTest(t, x), allocTest(t, x), allocTest(t, x)
	must(t, x.Write(a, pageByte(1)))
	x.SetRoot(a)
	s := x.Savepoint()
	must(t, x.Write(a, pageByte(2)))
	must(t, x.Free(b))
	must(t, x.Free(c))
	d := allocTest(t, x)
	if d != b {
		t.Fatal(d)
	}
	x.SetRoot(d)
	allocTest(t, x)
	must(t, x.Restore(s))
	readEquals(t, x, a, 1)
	readEquals(t, x, b, 0)
	if x.Root() != a || x.PageCount() != 4 {
		t.Fatal("savepoint metadata")
	}
	must(t, x.Write(a, pageByte(3)))
	must(t, x.Restore(s))
	readEquals(t, x, a, 1)
	must(t, x.Free(a))
	if e := x.Commit(); !errors.Is(e, ErrPage) {
		t.Fatal("committed free root", e)
	}
	must(t, x.Restore(s))
	must(t, x.Free(b))
	must(t, x.Free(c))
	must(t, x.Commit())
	must(t, p.Close())
	p = openTest(t, path)
	x = beginTest(t, p)
	if _, e := x.Read(b); !errors.Is(e, ErrPage) {
		t.Fatal(e)
	}
	if e := x.Free(b); !errors.Is(e, ErrPage) {
		t.Fatal(e)
	}
	if id := allocTest(t, x); id != b {
		t.Fatal(id)
	}
	readEquals(t, x, b, 0)
	must(t, x.Commit())
	x = beginTest(t, p)
	if e := x.Restore(s); e == nil {
		t.Fatal("foreign snapshot accepted")
	}
	if id := allocTest(t, x); id != c {
		t.Fatal(id)
	}
	must(t, x.Free(c))
	must(t, x.Rollback())
	must(t, p.Close())
	p = openTest(t, path)
	x = beginTest(t, p)
	if id := allocTest(t, x); id != c {
		t.Fatal("rollback lost free page", id)
	}
}

func TestValidationAndCache(t *testing.T) {
	p := openTest(t, filepath.Join(t.TempDir(), "db"))
	x := beginTest(t, p)
	if _, e := p.Begin(); !errors.Is(e, ErrBusy) {
		t.Fatal(e)
	}
	for i := 0; i < CachePages+10; i++ {
		allocTest(t, x)
	}
	must(t, x.Commit())
	x = beginTest(t, p)
	for id := uint32(1); id < x.PageCount(); id++ {
		readEquals(t, x, id, 0)
	}
	if len(p.cache) > CachePages || len(p.order) > CachePages {
		t.Fatal("unbounded cache")
	}
	for _, id := range []uint32{0, x.PageCount(), ^uint32(0)} {
		if _, e := x.Read(id); !errors.Is(e, ErrPage) {
			t.Fatal(e)
		}
		if e := x.Write(id, pageByte(1)); !errors.Is(e, ErrPage) {
			t.Fatal(e)
		}
	}
	if e := x.Write(1, []byte{1}); e == nil {
		t.Fatal("short page accepted")
	}
	x.SetRoot(x.PageCount())
	if e := x.Commit(); !errors.Is(e, ErrPage) {
		t.Fatal(e)
	}
	x.SetRoot(1)
	must(t, x.Commit())
	x = beginTest(t, p)
	must(t, p.Close())
	if _, e := x.Alloc(); !errors.Is(e, ErrClosed) {
		t.Fatal(e)
	}
	if _, e := p.Begin(); !errors.Is(e, ErrClosed) {
		t.Fatal(e)
	}
	must(t, p.Close())
}

func TestLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	p := openTest(t, path)
	if q, e := Open(path); !errors.Is(e, ErrLocked) {
		if q != nil {
			q.Close()
		}
		t.Fatal(e)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestLockChild$")
	cmd.Env = append(os.Environ(), "GBASE_LOCK_PATH="+path)
	if out, e := cmd.CombinedOutput(); e != nil {
		t.Fatalf("child: %v %s", e, out)
	}
	must(t, p.Close())
	openTest(t, path)
}

func TestLockChild(t *testing.T) {
	if path := os.Getenv("GBASE_LOCK_PATH"); path != "" {
		if p, e := Open(path); !errors.Is(e, ErrLocked) {
			if p != nil {
				p.Close()
			}
			t.Fatalf("expected lock: %v", e)
		}
	}
}

var faultPoints = []string{"journal-created", "journal-body-written", "journal-body-synced", "journal-ready-written", "journal-ready-synced", "journal-dir-synced", "db-page-written", "db-synced", "journal-removed", "removal-dir-synced"}

func seed(t *testing.T, path string) *Pager {
	t.Helper()
	p := openTest(t, path)
	x := beginTest(t, p)
	a, b := allocTest(t, x), allocTest(t, x)
	must(t, x.Write(a, pageByte(11)))
	must(t, x.Write(b, pageByte(22)))
	x.SetRoot(a)
	must(t, x.Commit())
	return p
}

func change(t *testing.T, p *Pager) *Tx {
	t.Helper()
	x := beginTest(t, p)
	must(t, x.Write(1, pageByte(33)))
	x.SetRoot(0)
	must(t, x.Free(2))
	id := allocTest(t, x)
	must(t, x.Write(id, pageByte(44)))
	id = allocTest(t, x)
	must(t, x.Write(id, pageByte(55)))
	x.SetRoot(id)
	return x
}

func TestCommitFaultRecovery(t *testing.T) {
	for _, point := range faultPoints {
		t.Run(point, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "db")
			p := seed(t, path)
			x := change(t, p)
			failure := errors.New("injected failure")
			p.Fault = func(s string) error {
				if s == point {
					return failure
				}
				return nil
			}
			if e := x.Commit(); !errors.Is(e, failure) {
				t.Fatal(e)
			}
			if _, e := p.Begin(); !errors.Is(e, ErrPoisoned) {
				t.Fatal(e)
			}
			must(t, p.Close())
			p = openTest(t, path)
			x = beginTest(t, p)
			committed := point == "journal-removed" || point == "removal-dir-synced"
			if committed {
				readEquals(t, x, 1, 33)
				readEquals(t, x, 2, 44)
				readEquals(t, x, 3, 55)
				if x.PageCount() != 4 || x.Root() != 3 {
					t.Fatal("committed metadata")
				}
			} else {
				readEquals(t, x, 1, 11)
				readEquals(t, x, 2, 22)
				if x.PageCount() != 3 || x.Root() != 1 {
					t.Fatal("recovery metadata")
				}
			}
			must(t, x.Rollback())
			if _, e := os.Stat(path + "-journal"); !errors.Is(e, os.ErrNotExist) {
				t.Fatal(e)
			}
		})
	}
}

func TestCrashRecovery(t *testing.T) {
	for _, point := range []string{"journal-created", "journal-ready-synced", "db-page-written", "db-synced"} {
		t.Run(point, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "db")
			p := seed(t, path)
			must(t, p.Close())
			cmd := exec.Command(os.Args[0], "-test.run=^TestCrashChild$")
			cmd.Env = append(os.Environ(), "GBASE_CRASH_PATH="+path, "GBASE_CRASH_POINT="+point)
			e := cmd.Run()
			var exit *exec.ExitError
			if !errors.As(e, &exit) || exit.ExitCode() != 73 {
				t.Fatalf("child exit: %v", e)
			}
			p = openTest(t, path)
			x := beginTest(t, p)
			readEquals(t, x, 1, 11)
			readEquals(t, x, 2, 22)
			if x.Root() != 1 || x.PageCount() != 3 {
				t.Fatal("crash metadata")
			}
		})
	}
}

func TestCrashChild(t *testing.T) {
	if path := os.Getenv("GBASE_CRASH_PATH"); path != "" {
		p := openTest(t, path)
		x := change(t, p)
		p.Fault = func(s string) error {
			if s == os.Getenv("GBASE_CRASH_POINT") {
				os.Exit(73)
			}
			return nil
		}
		must(t, x.Commit())
		t.Fatal("crash hook missed")
	}
}

func TestCorruption(t *testing.T) {
	for _, kind := range []string{"header-crc", "header-version", "header-size", "length", "free-crc", "free-cycle", "root-free", "journal-header", "journal-body", "journal-truncated", "journal-extra", "journal-duplicate"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "db")
			p := seed(t, path)
			if kind == "free-crc" || kind == "free-cycle" || kind == "root-free" {
				x := beginTest(t, p)
				must(t, x.Free(2))
				must(t, x.Commit())
			}
			journal := len(kind) >= 7 && kind[:7] == "journal"
			if journal {
				x := change(t, p)
				p.Fault = func(s string) error {
					if s == "db-synced" {
						return errors.New("stop")
					}
					return nil
				}
				if x.Commit() == nil {
					t.Fatal("missing failure")
				}
			}
			must(t, p.Close())
			target := path
			if journal {
				target += "-journal"
			}
			b, e := os.ReadFile(target)
			must(t, e)
			switch kind {
			case "header-crc":
				b[20] ^= 1
			case "header-version":
				put(b, 8, 99)
				seal(b[:PageSize])
			case "header-size":
				put(b, 12, 512)
				seal(b[:PageSize])
			case "length":
				b = b[:len(b)-1]
			case "free-crc":
				b[2*PageSize+9] ^= 1
			case "free-cycle":
				put(b[2*PageSize:], 8, 2)
				seal(b[2*PageSize:])
			case "root-free":
				put(b, 20, 2)
				seal(b[:PageSize])
			case "journal-header":
				b[16] ^= 1
			case "journal-body":
				b[PageSize+10] ^= 1
			case "journal-truncated":
				b = b[:len(b)-1]
			case "journal-extra":
				b = append(b, 0)
			case "journal-duplicate":
				put(b[PageSize+(PageSize+8):], 0, 0)
				record := b[PageSize+(PageSize+8) : PageSize+2*(PageSize+8)]
				put(record, PageSize+4, checksum(record[:PageSize+4]))
				put(b, 32, checksum(b[PageSize:]))
				seal(b[:PageSize])
			}
			must(t, os.WriteFile(target, b, 0o600))
			before, e := os.ReadFile(path)
			must(t, e)
			q, e := Open(path)
			if q != nil {
				q.Close()
			}
			if !errors.Is(e, ErrCorrupt) {
				t.Fatalf("expected corruption: %v", e)
			}
			after, e := os.ReadFile(path)
			must(t, e)
			if !bytes.Equal(before, after) {
				t.Fatal("corrupt open modified database")
			}
		})
	}
}

func TestOwnership(t *testing.T) {
	p := openTest(t, filepath.Join(t.TempDir(), "db"))
	x := beginTest(t, p)
	must(t, x.CheckOwnership(nil))
	a, b := allocTest(t, x), allocTest(t, x)
	must(t, x.CheckOwnership([]uint32{a, b}))
	for _, ids := range [][]uint32{{a}, {a, a}, {a, 0}, {a, b, 3}} {
		if e := x.CheckOwnership(ids); !errors.Is(e, ErrCorrupt) {
			t.Fatal(e)
		}
	}
	must(t, x.Free(b))
	must(t, x.CheckOwnership([]uint32{a}))
	if e := x.CheckOwnership([]uint32{a, b}); !errors.Is(e, ErrCorrupt) {
		t.Fatal(e)
	}
}

func TestIncompleteJournal(t *testing.T) {
	for _, kind := range []string{"empty", "partial-header", "bad-header-crc", "partial-body"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "db")
			p := seed(t, path)
			must(t, p.Close())
			b := journalHeader(3*PageSize, 2, 0, 0)
			switch kind {
			case "empty":
				b = nil
			case "partial-header":
				b = b[:40]
			case "bad-header-crc":
				b[PageSize-1] ^= 1
			case "partial-body":
				b = append(b, 1, 2, 3)
			}
			must(t, os.WriteFile(path+"-journal", b, 0o600))
			p = openTest(t, path)
			x := beginTest(t, p)
			readEquals(t, x, 1, 11)
			readEquals(t, x, 2, 22)
			if _, e := os.Stat(path + "-journal"); !errors.Is(e, os.ErrNotExist) {
				t.Fatal(e)
			}
		})
	}
}

func TestFreePages(t *testing.T) {
	p := openTest(t, filepath.Join(t.TempDir(), "db"))
	x := beginTest(t, p)
	a, b := allocTest(t, x), allocTest(t, x)
	must(t, x.Free(b))
	must(t, x.Free(a))
	pages := x.FreePages()
	if len(pages) != 2 || pages[0] != a || pages[1] != b {
		t.Fatal(pages)
	}
	pages[0] = 99
	if x.FreePages()[0] != a {
		t.Fatal("aliased free list")
	}
	if e := x.Write(a, pageByte(1)); !errors.Is(e, ErrPage) {
		t.Fatal("write to free page", e)
	}
	must(t, x.Commit())
	must(t, p.Close())
	p = openTest(t, p.path)
	x = beginTest(t, p)
	if len(x.FreePages()) != 2 {
		t.Fatal("free list not persisted")
	}
}

func TestSymlinkRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	p := seed(t, path)
	must(t, p.Close())
	alias := path + "-alias"
	must(t, os.Symlink(path, alias))
	p = openTest(t, alias)
	x := change(t, p)
	p.Fault = func(s string) error {
		if s == "db-synced" {
			return errors.New("stop")
		}
		return nil
	}
	if x.Commit() == nil {
		t.Fatal("missing fault")
	}
	must(t, p.Close())
	p = openTest(t, path)
	x = beginTest(t, p)
	readEquals(t, x, 1, 11)
	readEquals(t, x, 2, 22)
}

func TestPageChecksum(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	p := seed(t, path)
	must(t, p.Close())
	f, e := os.OpenFile(path, os.O_RDWR, 0o600)
	must(t, e)
	must(t, writeAt(f, []byte{99}, PageSize+100))
	must(t, f.Close())
	p = openTest(t, path)
	x := beginTest(t, p)
	if _, e = x.Read(1); !errors.Is(e, ErrCorrupt) {
		t.Fatal("corrupt page read", e)
	}
	must(t, x.Write(1, pageByte(1)))
	if e = x.Commit(); !errors.Is(e, ErrCorrupt) {
		t.Fatal("corrupt original journaled", e)
	}
	if _, e = p.Begin(); !errors.Is(e, ErrPoisoned) {
		t.Fatal(e)
	}
}

func TestSavepointJournalRelease(t *testing.T) {
	p := openTest(t, filepath.Join(t.TempDir(), "db"))
	x := beginTest(t, p)
	a := allocTest(t, x)
	must(t, x.Write(a, pageByte(1)))
	outer := x.Savepoint()
	must(t, x.Write(a, pageByte(2)))
	inner := x.Savepoint()
	must(t, x.Write(a, pageByte(3)))
	must(t, x.Restore(inner))
	readEquals(t, x, a, 2)
	must(t, x.Restore(outer))
	readEquals(t, x, a, 1)
	// inner was taken after outer's mark was rewound, so it can no longer be applied.
	must(t, x.Write(a, pageByte(4)))
	if e := x.Restore(inner); e == nil {
		t.Fatal("stale savepoint accepted")
	}
	x.Release(inner)
	x.Release(outer)
	if len(x.undo) != 0 {
		t.Fatal("journal not discarded after last release")
	}
	// Released savepoints are rejected and mutations are no longer journaled.
	must(t, x.Write(a, pageByte(5)))
	if len(x.undo) != 0 {
		t.Fatal("journaling without savepoint")
	}
	if e := x.Restore(outer); e == nil {
		t.Fatal("released savepoint accepted")
	}
	must(t, x.Commit())
}

func TestSetCachePages(t *testing.T) {
	p, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	tx, _ := p.Begin()
	var ids []uint32
	for i := 0; i < 20; i++ {
		id, err := tx.Alloc()
		if err != nil {
			t.Fatal(err)
		}
		if err = tx.Write(id, make([]byte, PageSize)); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err = p.SetCachePages(4); !errors.Is(err, ErrBusy) {
		t.Fatalf("resize during transaction: %v", err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = p.SetCachePages(0); err == nil {
		t.Fatal("zero-page cache accepted")
	}
	for _, n := range []int{4, 100} {
		if err = p.SetCachePages(n); err != nil {
			t.Fatal(err)
		}
		tx, _ = p.Begin()
		for _, id := range ids {
			if _, err = tx.Read(id); err != nil {
				t.Fatal(err)
			}
		}
		tx.Rollback()
		if want := min(n, len(ids)); len(p.cache) != want || len(p.order) != want {
			t.Fatalf("limit %d: cache %d order %d", n, len(p.cache), len(p.order))
		}
	}
	if err = p.SetCachePages(2); err != nil || len(p.cache) != 2 {
		t.Fatalf("shrink: %v %d", err, len(p.cache))
	}
}
