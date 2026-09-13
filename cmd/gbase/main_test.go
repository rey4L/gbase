package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSplitter(t *testing.T) {
	tests := []struct {
		name, input string
		want        []string
		bad         bool
	}{
		{"multiple", "SELECT 1;SELECT 2; SELECT 3", []string{"SELECT 1", "SELECT 2", "SELECT 3"}, false},
		{"quotes", "SELECT 'a;''b', \"c;\"\"d\", `e;``f`, [g;]]h];", []string{"SELECT 'a;''b', \"c;\"\"d\", `e;``f`, [g;]]h]"}, false},
		{"comments", "-- ignored; '\n SELECT/* ; */1; /* more; */ SELECT '--;/*'; -- EOF", []string{"SELECT 1", "SELECT '--;/*'"}, false},
		{"commands", "-- heading\n.tables\n.schema; .check\n.exit", []string{".tables", ".schema", ".check", ".exit"}, false},
		{"command comments", ".tables -- heading;\n.schema /* ; */\n.exit -- EOF", []string{".tables", ".schema", ".exit"}, false},
		{"multiline", "SELECT 'line\n;two';\nSELECT\n 2;", []string{"SELECT 'line\n;two'", "SELECT\n 2"}, false},
		{"empty", ";; -- only comments\n /* nothing; */", nil, false},
		{"quote EOF", "SELECT 'broken", nil, true},
		{"comment EOF", "SELECT /* broken", nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sp := splitter{r: bufio.NewReader(strings.NewReader(tt.input))}
			var got []string
			var err error
			for {
				var statement string
				statement, err = sp.next()
				if err != nil {
					break
				}
				got = append(got, statement)
			}
			if tt.bad {
				if err == nil || errors.Is(err, io.EOF) {
					t.Fatalf("expected malformed input error, got %v", err)
				}
				return
			}
			if !errors.Is(err, io.EOF) {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %#v, want %#v", got, tt.want)
			}
		})
	}
}

func script(t *testing.T, path, input string) (int, string, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out, stderr bytes.Buffer
	code := run(ctx, []string{path}, strings.NewReader(input), &out, &stderr, false)
	return code, out.String(), stderr.String()
}

func TestScriptRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "roundtrip.db")
	code, out, stderr := script(t, path, `
-- Script statements can share lines.
CREATE TABLE items (id INTEGER PRIMARY KEY, name TEXT, data BLOB, absent TEXT);
BEGIN TRANSACTION; INSERT INTO items VALUES (1, 'a;''b', X'00ff', NULL);
SELECT * FROM items; COMMIT;
BEGIN; INSERT INTO items VALUES (2, 'discard', X'', NULL); ROLLBACK;
.tables
.schema
.check
`)
	if code != 0 {
		t.Fatalf("code %d: %s", code, stderr)
	}
	for _, want := range []string{"id", "name", "a;'b", "X'00FF'", "NULL", "items", "CREATE TABLE", "ok"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %s", want, out)
		}
	}
	code, out, stderr = script(t, path, "SELECT * FROM items;")
	if code != 0 || !strings.Contains(out, "a;'b") || strings.Contains(out, "discard") {
		t.Fatalf("reopen: %d %s %s", code, out, stderr)
	}
}

func TestRollbackAtEOFExitAndError(t *testing.T) {
	for _, ending := range []string{"", ".exit\n", "INSERT INTO missing VALUES (1);", "SELECT 'unterminated"} {
		t.Run(ending, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "rollback.db")
			code, _, stderr := script(t, path, "CREATE TABLE t (id INTEGER); BEGIN; INSERT INTO t VALUES (42);"+ending)
			wantError := strings.HasPrefix(ending, "INSERT") || strings.HasPrefix(ending, "SELECT")
			if (code != 0) != wantError {
				t.Fatalf("code %d: %s", code, stderr)
			}
			code, out, stderr := script(t, path, "SELECT * FROM t;")
			if code != 0 || strings.Contains(out, "42") {
				t.Fatalf("rollback: %d %s %s", code, out, stderr)
			}
		})
	}
}

func TestTransactionErrors(t *testing.T) {
	for _, input := range []string{"BEGIN; BEGIN;", "COMMIT;", "ROLLBACK;", "BEGIN nonsense;", "BEGIN; .check\n", "BEGIN; .schema\n", "BEGIN; .tables\n", ".unknown\n", ".exit extra\n"} {
		t.Run(input, func(t *testing.T) {
			code, _, stderr := script(t, filepath.Join(t.TempDir(), "errors.db"), input)
			if code == 0 || stderr == "" {
				t.Fatalf("expected error: %d %q", code, stderr)
			}
		})
	}
}

func TestInteractiveContinuesAfterError(t *testing.T) {
	var out, stderr bytes.Buffer
	code := run(context.Background(), []string{filepath.Join(t.TempDir(), "interactive.db")}, strings.NewReader("invalid; .check\n.exit\n"), &out, &stderr, true)
	if code != 1 || !strings.Contains(out.String(), "gbase> ") || !strings.Contains(out.String(), "ok") || stderr.Len() == 0 {
		t.Fatalf("%d %s %s", code, &out, &stderr)
	}
}

func TestCancellationWhileWaitingForInput(t *testing.T) {
	r, w := io.Pipe()
	defer r.Close()
	defer w.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	path := filepath.Join(t.TempDir(), "cancel.db")
	input := &waitingReader{Reader: r, waiting: make(chan struct{})}
	go func() { done <- run(ctx, []string{path}, input, io.Discard, io.Discard, false) }()
	if _, err := io.WriteString(w, "CREATE TABLE t (id INTEGER); BEGIN; INSERT INTO t VALUES (42);\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-input.waiting:
	case <-time.After(5 * time.Second):
		t.Fatal("script did not reach input wait")
	}
	cancel()
	select {
	case code := <-done:
		if code == 0 {
			t.Fatal("cancellation succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation blocked on input")
	}
	code, out, stderr := script(t, path, "SELECT * FROM t;")
	if code != 0 || strings.Contains(out, "42") {
		t.Fatalf("cancel rollback: %d %s %s", code, out, stderr)
	}
}

// Signal the second buffer fill, after all first-buffer statements executed.
type waitingReader struct {
	io.Reader
	waiting chan struct{}
	reads   int
}

func (r *waitingReader) Read(p []byte) (int, error) {
	r.reads++
	if r.reads == 2 {
		close(r.waiting)
	}
	return r.Reader.Read(p)
}

func TestFlags(t *testing.T) {
	for _, tt := range []struct {
		args []string
		want int
	}{{[]string{"-h"}, 0}, {nil, 2}, {[]string{"a", "b"}, 2}, {[]string{"-unknown"}, 2}} {
		var stderr bytes.Buffer
		if got := run(context.Background(), tt.args, strings.NewReader(""), io.Discard, &stderr, false); got != tt.want {
			t.Fatalf("%v: got %d", tt.args, got)
		}
		if stderr.Len() == 0 {
			t.Fatal("missing usage")
		}
	}
}

func TestDisplay(t *testing.T) {
	for _, tt := range []struct {
		value any
		want  string
	}{{nil, "NULL"}, {[]byte{0, 255}, "X'00FF'"}, {int64(12), "12"}, {1.5, "1.5"}, {"a\tb\nc\\d", "a\\tb\\nc\\\\d"}} {
		if got := display(tt.value); got != tt.want {
			t.Errorf("got %q, want %q", got, tt.want)
		}
	}
}
