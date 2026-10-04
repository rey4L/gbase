package gbase

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestBackupFileUnderConcurrentWrites(t *testing.T) {
	dir := t.TempDir()
	db, e := Open(filepath.Join(dir, "live.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	execTest(t, db, "CREATE TABLE t (id INTEGER PRIMARY KEY, tag TEXT, body TEXT)")
	execTest(t, db, "CREATE INDEX tt ON t(tag)")
	big := string(bytes.Repeat([]byte("x"), 5000))
	for i := 0; i < 200; i++ {
		execTest(t, db, "INSERT INTO t (tag, body) VALUES (?, ?)", "k", big)
	}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			// Each insert is a pair so a consistent copy always holds an even count.
			tx, e := db.Begin(bg)
			if e != nil {
				t.Error(e)
				return
			}
			for j := 0; j < 2; j++ {
				if _, e = tx.Exec(bg, "INSERT INTO t (tag, body) VALUES (?, ?)", "k", big); e != nil {
					t.Error(e)
				}
			}
			if e = tx.Commit(); e != nil {
				t.Error(e)
			}
		}
	}()
	var paths []string
	for i := 0; i < 5; i++ {
		p := filepath.Join(dir, "copy.db")
		if i%2 == 1 {
			p = filepath.Join(dir, "other.db")
		}
		if e := db.BackupFile(bg, p); e != nil {
			t.Fatal(e)
		}
		paths = append(paths, p)
		copyDB, e := Open(p)
		if e != nil {
			t.Fatal(e)
		}
		if e = copyDB.Check(bg); e != nil {
			t.Fatal(e)
		}
		n := queryTest(t, copyDB, "SELECT COUNT(*) FROM t")[0][0].(int64)
		if n < 200 || n%2 != 0 {
			t.Fatalf("inconsistent copy: %d rows", n)
		}
		copyDB.Close()
	}
	close(stop)
	wg.Wait()
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		if name := entry.Name(); name != "live.db" && name != "copy.db" && name != "other.db" {
			t.Fatalf("leftover file %s", name)
		}
	}
}

func TestBackupRefusals(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "live.db")
	db, e := Open(live)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	execTest(t, db, "CREATE TABLE t (id INTEGER PRIMARY KEY)")
	if e = db.BackupFile(bg, live); e == nil {
		t.Fatal("backup onto the live database accepted")
	}
	link := filepath.Join(dir, "link.db")
	if e = os.Symlink(live, link); e != nil {
		t.Fatal(e)
	}
	if e = db.BackupFile(bg, link); e == nil {
		t.Fatal("backup onto a symlink to the live database accepted")
	}
	ctx, cancel := context.WithCancel(bg)
	cancel()
	if e = db.BackupFile(ctx, filepath.Join(dir, "c.db")); !errors.Is(e, context.Canceled) {
		t.Fatalf("canceled backup: %v", e)
	}
	if _, e = os.Stat(filepath.Join(dir, "c.db")); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("canceled backup left a file")
	}
	// The database stays usable after refused and canceled backups.
	execTest(t, db, "INSERT INTO t VALUES (1)")
	var buf bytes.Buffer
	if e = db.Backup(bg, &buf); e != nil || buf.Len()%4096 != 0 {
		t.Fatalf("Backup: %v, %d bytes", e, buf.Len())
	}
}
