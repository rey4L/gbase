package gbase

import (
	"path/filepath"
	"testing"
)

func benchmarkData(b *testing.B, indexed bool) *DB {
	b.Helper()
	db, e := Open(filepath.Join(b.TempDir(), "bench.db"))
	if e != nil {
		b.Fatal(e)
	}
	b.Cleanup(func() { db.Close() })
	if _, e = db.Exec(bg, "CREATE TABLE t (id INTEGER PRIMARY KEY, n INTEGER, body TEXT)"); e != nil {
		b.Fatal(e)
	}
	tx, e := db.Begin(bg)
	if e != nil {
		b.Fatal(e)
	}
	for i := 0; i < 2000; i++ {
		if _, e = tx.Exec(bg, "INSERT INTO t VALUES (?,?,?)", i, i, "value"); e != nil {
			b.Fatal(e)
		}
	}
	if e = tx.Commit(); e != nil {
		b.Fatal(e)
	}
	if indexed {
		if _, e = db.Exec(bg, "CREATE INDEX by_n ON t(n)"); e != nil {
			b.Fatal(e)
		}
	}
	return db
}

func BenchmarkSecondaryLookup(b *testing.B) {
	for _, indexed := range []bool{false, true} {
		name := "scan"
		if indexed {
			name = "index"
		}
		b.Run(name, func(b *testing.B) {
			db := benchmarkData(b, indexed)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				r, e := db.Query(bg, "SELECT body FROM t WHERE n=?", i%2000)
				if e != nil {
					b.Fatal(e)
				}
				for r.Next() {
					r.Values()
				}
				if e = r.Err(); e != nil {
					b.Fatal(e)
				}
			}
		})
	}
}

func BenchmarkFullScan(b *testing.B) {
	db := benchmarkData(b, false)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r, e := db.Query(bg, "SELECT id,body FROM t")
		if e != nil {
			b.Fatal(e)
		}
		for r.Next() {
			r.Values()
		}
		if e = r.Err(); e != nil {
			b.Fatal(e)
		}
	}
}

func BenchmarkCommitBatch(b *testing.B) {
	db, e := Open(filepath.Join(b.TempDir(), "batch.db"))
	if e != nil {
		b.Fatal(e)
	}
	defer db.Close()
	if _, e = db.Exec(bg, "CREATE TABLE t (id INTEGER PRIMARY KEY, n INTEGER)"); e != nil {
		b.Fatal(e)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tx, e := db.Begin(bg)
		if e != nil {
			b.Fatal(e)
		}
		for n := 0; n < 100; n++ {
			if _, e = tx.Exec(bg, "INSERT INTO t (n) VALUES (?)", n); e != nil {
				b.Fatal(e)
			}
		}
		if e = tx.Commit(); e != nil {
			b.Fatal(e)
		}
		b.StopTimer()
		if _, e = db.Exec(bg, "DELETE FROM t"); e != nil {
			b.Fatal(e)
		}
		b.StartTimer()
	}
}
