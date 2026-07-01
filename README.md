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

Shell: `.tables`, `.schema`, `.check`, `.exit`. Scripts: `go run ./cmd/gbase demo.db < script.sql`.

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

Use `Begin(ctx)`, transaction `Exec`/`Query`, then `Commit` or `Rollback`. Close query cursors before issuing another operation.

```sh
go test ./...
go test -race ./...
go vet ./...
```

