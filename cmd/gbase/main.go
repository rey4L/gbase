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
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/rey4L/gbase"
	"github.com/rey4L/gbase/export"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	info, err := os.Stdin.Stat()
	interactive := err == nil && info.Mode()&os.ModeCharDevice != 0
	os.Exit(run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr, interactive))
}

func runExport(ctx context.Context, db *gbase.DB, dir string, d export.Dialect, out, stderr io.Writer) int {
	report, err := export.Export(ctx, db, dir, d)
	if cerr := db.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	for _, t := range report.Tables {
		fmt.Fprintf(out, "%s: %d rows\n", t.Name, t.Rows)
	}
	for _, w := range report.Warnings {
		fmt.Fprintln(out, "warning:", w)
	}
	fmt.Fprintf(out, "wrote %s; see %s\n", dir, filepath.Join(dir, "REPORT.md"))
	return 0
}

// run keeps streams injectable so scripts exercise the same path as the shell.
func run(ctx context.Context, args []string, in io.Reader, out, stderr io.Writer, interactive bool) (code int) {
	flags := flag.NewFlagSet("gbase", flag.ContinueOnError)
	flags.SetOutput(stderr)
	exportDir := flags.String("export", "", "write `DIR` with DDL, CSV data, and a load script instead of starting the shell")
	dialect := flags.String("dialect", "postgres", "export target: postgres or oracle")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "Usage: gbase file.db\n       gbase -export DIR [-dialect postgres|oracle] file.db\nSQL ends with ; (or script EOF). Commands: .tables .schema .check .backup FILE .exit\nBEGIN [TRANSACTION], COMMIT, ROLLBACK control transactions. EOF rolls back.")
		flags.PrintDefaults()
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
	if *exportDir != "" {
		return runExport(ctx, db, *exportDir, export.Dialect(*dialect), out, stderr)
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
	var editor *lineEditor
	var restore func() error
	if interactive {
		editor, restore, err = attachTerminal(readCtx, in, out)
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		if editor != nil {
			in = editor
		}
	}
	type input struct {
		statement string
		err       error
	}
	requests := make(chan struct{})
	inputs := make(chan input)
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
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
	defer func() {
		cancel()
		if restore != nil {
			// Stop terminal reads before restoring settings. This defer belongs in
			// run: main's os.Exit does not execute main's deferred functions.
			<-readDone
			if err := restore(); err != nil {
				fmt.Fprintln(stderr, "error:", err)
				code = 1
			}
		}
	}()
	reportCancellation := func() {
		cancel()
		if editor != nil {
			// Finish redraws before printing the cancellation message on a new row.
			<-readDone
			fmt.Fprintln(out)
		}
		fmt.Fprintln(stderr, "error:", ctx.Err())
	}
	for {
		if editor != nil {
			// The request channel hands prompt state to the input goroutine.
			editor.continuation = false
		}
		if interactive && editor == nil {
			if _, err := fmt.Fprint(out, "gbase> "); err != nil {
				fmt.Fprintln(stderr, "error:", err)
				return 1
			}
		}
		select {
		case <-ctx.Done():
			reportCancellation()
			return 1
		case requests <- struct{}{}:
		}
		var item input
		select {
		case <-ctx.Done():
			reportCancellation()
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
		if first == ".BACKUP" {
			if len(words) != 2 {
				return false, errors.New(".backup takes one file argument")
			}
			if s.tx != nil {
				return false, errors.New(".backup unavailable during an explicit transaction")
			}
			if err := s.db.BackupFile(ctx, words[1]); err != nil {
				return false, err
			}
			_, err := fmt.Fprintln(s.out, "ok")
			return false, err
		}
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
	if slices.Equal(rows.Columns(), queryPlanColumns) {
		return printPlan(out, rows)
	}
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

// queryPlanColumns identifies EXPLAIN QUERY PLAN output, which is drawn as a tree like the SQLite shell does.
var queryPlanColumns = []string{"id", "parent", "notused", "detail"}

func printPlan(out io.Writer, rows *gbase.Rows) error {
	type node struct {
		id, parent int64
		detail     string
	}
	var nodes []node
	for rows.Next() {
		v := rows.Values()
		id, _ := v[0].(int64)
		parent, _ := v[1].(int64)
		nodes = append(nodes, node{id, parent, display(v[3])})
	}
	if err := rows.Err(); err != nil {
		return err
	}
	var draw func(parent int64, prefix string) error
	draw = func(parent int64, prefix string) error {
		var kids []node
		for _, n := range nodes {
			if n.parent == parent {
				kids = append(kids, n)
			}
		}
		for i, n := range kids {
			branch, next := "|--", "|  "
			if i == len(kids)-1 {
				branch, next = "`--", "   "
			}
			if _, err := fmt.Fprintf(out, "%s%s%s\n", prefix, branch, n.detail); err != nil {
				return err
			}
			if err := draw(n.id, prefix+next); err != nil {
				return err
			}
		}
		return nil
	}
	if _, err := fmt.Fprintln(out, "QUERY PLAN"); err != nil {
		return err
	}
	return draw(0, "")
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
