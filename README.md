# gbase

Embedded ACID-compliant Go database. Zero external Go dependencies; own format, not SQLite-compatible. Linux, Go 1.27.1+.

```sh
go run ./cmd/gbase demo.db
```

```sql
CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT UNIQUE);
INSERT INTO users (name) VALUES ('Ada');
SELECT * FROM users;
```

Shell: `.tables`, `.schema`, `.check`, `.backup FILE`, `.exit`.

Export to PostgreSQL or Oracle: `go run ./cmd/gbase -export DIR -dialect postgres demo.db` writes DDL, CSV data, a load script, and `REPORT.md`, which lists data the target would reject or change (reserved names; for Oracle, `''` becoming NULL and partial-NULL duplicates in multi-column UNIQUE). Scripts: `go run ./cmd/gbase demo.db < script.sql`.

Go import: `github.com/rey4L/gbase`. In another local module, add `replace github.com/rey4L/gbase => ../gbase` to `go.mod`.

```go
db, err := gbase.Open("demo.db")
if err != nil { panic(err) }
defer db.Close()
rows, err := db.Query(context.Background(), "SELECT name FROM users WHERE id = ?", 1)
if err != nil { panic(err) }
defer rows.Close()
for rows.Next() { fmt.Println(rows.Values()) }
if err := rows.Err(); err != nil { panic(err) }
```

With `database/sql`, import `_ "github.com/rey4L/gbase/sqldriver"` and `sql.Open("gbase", "demo.db?busy_timeout=5s")`; see the package documentation for parameters, type mapping, and `Conn.Raw` access to `Backup`.

Parameters are `?` or named (`:name`, `@name`, `$name`; a repeated name is one parameter), not both in one statement.

Use `Begin(ctx)`, transaction `Exec`/`Query`, then `Commit` or `Rollback`. Close query cursors before issuing another operation. Only one transaction runs at a time; `SetBusyTimeout(d)` makes a waiting `Begin` fail with `ErrLocked` after `d` instead of waiting for its context.

`BackupFile(ctx, path)` atomically writes a consistent copy while the database stays open; writers wait for it. `Backup(ctx, w)` streams the same copy to any writer.

Indexes and UNIQUE constraints may span columns: `CREATE INDEX i ON t (a, b)`, `UNIQUE (a, b)` in `CREATE TABLE`. A NULL in any column exempts a row from uniqueness. Equalities on leading columns seek the index.

Column types are INTEGER, REAL, TEXT, and BLOB, plus BOOLEAN (stored as INTEGER 0 or 1), DATE (TEXT `YYYY-MM-DD`), and DATETIME or TIMESTAMP (TEXT, canonical UTC `2006-01-02T15:04:05.000000Z`, so text order is time order). `Rows.DeclTypes` reports declared result types.

Schema changes: `ALTER TABLE t ADD [COLUMN] def`, `DROP [COLUMN] c`, `RENAME TO name`, `RENAME [COLUMN] c TO name`. Each rewrites the table's rows, so it costs time proportional to the table.

```sh
go test ./...
go test -race ./...
go vet ./...
```

