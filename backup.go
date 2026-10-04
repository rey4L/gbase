package gbase

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
)

// Backup writes a consistent copy of the committed database to w. It holds
// the database for the duration, like any transaction, so writers wait.
// The copy can be opened directly with Open.
func (db *DB) Backup(ctx context.Context, w io.Writer) error {
	tx, e := db.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if tx.done {
		return ErrClosed
	}
	return tx.pages.Copy(contextWriter{ctx, w})
}

// BackupFile writes a backup to path atomically: it fills a temporary file in
// the same directory, syncs it, and renames it over path, so path never holds
// a partial backup. Backing up onto the open database itself is refused.
func (db *DB) BackupFile(ctx context.Context, path string) (err error) {
	if same, e := db.isFile(path); e != nil {
		return e
	} else if same {
		return fail("backup", "destination is the open database")
	}
	f, e := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if e != nil {
		return e
	}
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(f.Name())
		}
	}()
	if err = db.Backup(ctx, f); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func (db *DB) isFile(path string) (bool, error) {
	dst, e := os.Stat(path)
	if errors.Is(e, os.ErrNotExist) {
		return false, nil
	}
	if e != nil {
		return false, e
	}
	src, e := os.Stat(db.pager.Path())
	if e != nil {
		return false, e
	}
	return os.SameFile(src, dst), nil
}

type contextWriter struct {
	ctx context.Context
	w   io.Writer
}

func (c contextWriter) Write(b []byte) (int, error) {
	if e := c.ctx.Err(); e != nil {
		return 0, e
	}
	return c.w.Write(b)
}
