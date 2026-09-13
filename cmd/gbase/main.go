// gbase is a small SQL shell for the native gbase database.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"text/tabwriter"

	"github.com/rey4L/gbase"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	info, err := os.Stdin.Stat()
	interactive := err == nil && info.Mode()&os.ModeCharDevice != 0
	os.Exit(run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr, interactive))
}

// run keeps streams injectable so scripts exercise the same path as the shell.
func run(ctx context.Context, args []string, in io.Reader, out, stderr io.Writer, interactive bool) (code int) {
	flags := flag.NewFlagSet("gbase", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() {
		fmt.Fprintln(stderr, "Usage: gbase file.db\nSQL ends with ; (or script EOF). Commands: .tables .schema .check .exit\nBEGIN [TRANSACTION], COMMIT, ROLLBACK control transactions. EOF rolls back.")
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 1 {
		flags.Usage()
		return 2
	}
	db, err := gbase.Open(flags.Arg(0))
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	s := shell{db: db, out: out}
	defer func() {
		if s.tx != nil {
			if err := s.tx.Rollback(); err != nil {
				fmt.Fprintln(stderr, "error:", err)
				code = 1
			}
		}
		if err := db.Close(); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			code = 1
		}
	}()
	readCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	type input struct {
		statement string
		err       error
	}
	requests := make(chan struct{})
	inputs := make(chan input)
	go func() {
		sp := splitter{r: bufio.NewReader(in)}
		for {
			select {
			case <-readCtx.Done():
				return
			case <-requests:
			}
			statement, err := sp.next()
			select {
			case <-readCtx.Done():
				return
			case inputs <- input{statement, err}:
			}
			if err != nil {
				return
			}
		}
	}()
	for {
		if interactive {
			if _, err := fmt.Fprint(out, "gbase> "); err != nil {
				fmt.Fprintln(stderr, "error:", err)
				return 1
			}
		}
		select {
		case <-ctx.Done():
			fmt.Fprintln(stderr, "error:", ctx.Err())
			return 1
		case requests <- struct{}{}:
		}
		var item input
		select {
		case <-ctx.Done():
			fmt.Fprintln(stderr, "error:", ctx.Err())
			return 1
		case item = <-inputs:
		}
		if errors.Is(item.err, io.EOF) {
			return code
		}
		if item.err != nil {
			fmt.Fprintln(stderr, "error:", item.err)
			return 1
		}
		exit, err := s.execute(ctx, item.statement)
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			code = 1
			if !interactive {
				return code
			}
		}
		if exit {
			return code
		}
	}
}

type shell struct {
	db  *gbase.DB
	tx  *gbase.Tx
	out io.Writer
}

func (s *shell) execute(ctx context.Context, statement string) (bool, error) {
	words := strings.Fields(statement)
	if len(words) == 0 {
		return false, nil
	}
	first := strings.ToUpper(words[0])
	if strings.HasPrefix(first, ".") {
		if len(words) != 1 {
			return false, fmt.Errorf("%s takes no arguments", words[0])
		}
		switch first {
		case ".EXIT":
			return true, nil
		case ".TABLES", ".SCHEMA", ".CHECK":
			if s.tx != nil {
				return false, fmt.Errorf("%s unavailable during an explicit transaction", words[0])
			}
			if first == ".CHECK" {
				if err := s.db.Check(ctx); err != nil {
					return false, err
				}
				_, err := fmt.Fprintln(s.out, "ok")
				return false, err
			}
			var lines []string
			var err error
			if first == ".TABLES" {
				lines, err = s.db.Tables(ctx)
			} else {
				lines, err = s.db.Schema(ctx)
			}
			if err != nil {
				return false, err
			}
			for _, line := range lines {
				if _, err := fmt.Fprintln(s.out, line); err != nil {
					return false, err
				}
			}
			return false, nil
		default:
			return false, fmt.Errorf("unknown command %s", words[0])
		}
	}
	switch first {
	case "BEGIN", "COMMIT", "ROLLBACK":
		if len(words) > 2 || len(words) == 2 && !strings.EqualFold(words[1], "TRANSACTION") {
			return false, fmt.Errorf("expected %s [TRANSACTION]", first)
		}
		if first == "BEGIN" {
			if s.tx != nil {
				return false, errors.New("transaction already active")
			}
			tx, err := s.db.Begin(ctx)
			s.tx = tx
			return false, err
		}
		if s.tx == nil {
			return false, errors.New("no active transaction")
		}
		var err error
		if first == "COMMIT" {
			err = s.tx.Commit()
		} else {
			err = s.tx.Rollback()
		}
		if err == nil {
			s.tx = nil
		}
		return false, err
	case "SELECT", "EXPLAIN":
		var rows *gbase.Rows
		var err error
		if s.tx != nil {
			rows, err = s.tx.Query(ctx, statement)
		} else {
			rows, err = s.db.Query(ctx, statement)
		}
		if err != nil {
			return false, err
		}
		return false, printRows(s.out, rows)
	default:
		var result gbase.Result
		var err error
		if s.tx != nil {
			result, err = s.tx.Exec(ctx, statement)
		} else {
			result, err = s.db.Exec(ctx, statement)
		}
		if err != nil {
			return false, err
		}
		_, err = fmt.Fprintf(s.out, "%d row(s) affected; last insert ID: %d\n", result.RowsAffected, result.LastInsertID)
		return false, err
	}
}

func printRows(out io.Writer, rows *gbase.Rows) (err error) {
	defer func() { err = errors.Join(err, rows.Close()) }()
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	write := func(cells []string) error { _, err := fmt.Fprintln(w, strings.Join(cells, "\t")); return err }
	columns := rows.Columns()
	for i := range columns {
		columns[i] = display(columns[i])
	}
	if err := write(columns); err != nil {
		return err
	}
	for rows.Next() {
		values := rows.Values()
		cells := make([]string, len(values))
		for i, value := range values {
			cells[i] = display(value)
		}
		if err := write(cells); err != nil {
			return err
		}
	}
	return errors.Join(rows.Err(), w.Flush())
}

func display(value gbase.Value) string {
	switch v := value.(type) {
	case nil:
		return "NULL"
	case []byte:
		return fmt.Sprintf("X'%X'", v)
	case string:
		return strings.NewReplacer("\\", "\\\\", "\t", "\\t", "\n", "\\n", "\r", "\\r", "\v", "\\v", "\f", "\\f").Replace(v)
	default:
		return fmt.Sprint(v)
	}
}

// splitter removes SQL comments, retaining token boundaries, and consumes one
// statement at a time. SQL doubled quotes escape quotes; backslash does not.
type splitter struct{ r *bufio.Reader }

func (s *splitter) next() (string, error) {
	var b strings.Builder
	var quote byte
	lineComment, blockComment, meta, content := false, false, false, false
	for {
		c, err := s.r.ReadByte()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				return "", err
			}
			if quote != 0 {
				return "", errors.New("unterminated quoted value")
			}
			if blockComment {
				return "", errors.New("unterminated SQL comment")
			}
			if content {
				return strings.TrimSpace(b.String()), nil
			}
			return "", io.EOF
		}
		if lineComment {
			if c == '\n' {
				lineComment = false
				if meta && content {
					return strings.TrimSpace(b.String()), nil
				}
				b.WriteByte('\n')
			}
			continue
		}
		if blockComment {
			if c == '*' {
				if peek, _ := s.r.Peek(1); len(peek) == 1 && peek[0] == '/' {
					s.r.ReadByte()
					blockComment = false
				}
			}
			continue
		}
		if quote != 0 {
			b.WriteByte(c)
			if c == quote {
				if peek, _ := s.r.Peek(1); len(peek) == 1 && peek[0] == quote {
					s.r.ReadByte()
					b.WriteByte(c)
				} else {
					quote = 0
				}
			}
			continue
		}
		if c == '-' || c == '/' {
			if peek, _ := s.r.Peek(1); len(peek) == 1 && (c == '-' && peek[0] == '-' || c == '/' && peek[0] == '*') {
				s.r.ReadByte()
				b.WriteByte(' ')
				lineComment = c == '-'
				blockComment = c == '/'
				continue
			}
		}
		if c == ';' || meta && c == '\n' {
			if content {
				return strings.TrimSpace(b.String()), nil
			}
			b.Reset()
			continue
		}
		if c == '\'' || c == '"' || c == '`' {
			quote = c
		}
		if c == '[' {
			quote = ']'
		}
		if c != ' ' && c != '\t' && c != '\r' && c != '\n' {
			if !content {
				meta = c == '.'
			}
			content = true
		}
		b.WriteByte(c)
	}
}
