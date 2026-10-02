package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
	"unicode"
)

const (
	historyLimit = 500
	clearScreen  = "\x1b[2J\x1b[H"
)

// lineEditor is an io.Reader adapter: the SQL splitter still receives ordinary
// newline-terminated text, while terminal editing stays in this input layer.
type lineEditor struct {
	ctx          context.Context
	input        *bufio.Reader
	output       io.Writer
	columns      func() int
	history      []string
	pending      []byte
	continuation bool
	eof          bool
}

type editLine struct {
	text         []rune
	cursor       int
	historyIndex int
	draft        []rune
}

func newLineEditor(ctx context.Context, input io.Reader, output io.Writer, columns func() int) *lineEditor {
	return &lineEditor{ctx: ctx, input: bufio.NewReader(input), output: output, columns: columns}
}

func (e *lineEditor) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(e.pending) == 0 {
		if e.eof {
			return 0, io.EOF
		}
		line, err := e.readLine()
		if err != nil {
			return 0, err
		}
		e.pending = []byte(line + "\n")
	}
	n := copy(p, e.pending)
	e.pending = e.pending[n:]
	return n, nil
}

func (e *lineEditor) remember(line string) {
	if strings.TrimSpace(line) == "" {
		return
	}
	if n := len(e.history); n > 0 && e.history[n-1] == line {
		return
	}
	if len(e.history) == historyLimit {
		copy(e.history, e.history[1:])
		e.history = e.history[:historyLimit-1]
	}
	e.history = append(e.history, line)
}

func (l *editLine) insert(r rune) {
	l.text = append(l.text, 0)
	copy(l.text[l.cursor+1:], l.text[l.cursor:])
	l.text[l.cursor] = r
	l.cursor++
}

func (l *editLine) backspace() {
	if l.cursor == 0 {
		return
	}
	l.cursor--
	l.delete()
}

func (l *editLine) delete() {
	if l.cursor == len(l.text) {
		return
	}
	copy(l.text[l.cursor:], l.text[l.cursor+1:])
	l.text = l.text[:len(l.text)-1]
}

func (l *editLine) previous(history []string) {
	if l.historyIndex == 0 {
		return
	}
	// History entries are immutable. Save the unfinished draft before browsing,
	// and edit a copy so changing a recalled line cannot rewrite past input.
	if l.historyIndex == len(history) {
		l.draft = append([]rune(nil), l.text...)
	}
	l.historyIndex--
	l.text = []rune(history[l.historyIndex])
	l.cursor = len(l.text)
}

func (l *editLine) next(history []string) {
	if l.historyIndex == len(history) {
		return
	}
	l.historyIndex++
	if l.historyIndex == len(history) {
		l.text = append([]rune(nil), l.draft...)
	} else {
		l.text = []rune(history[l.historyIndex])
	}
	l.cursor = len(l.text)
}

func (e *lineEditor) readLine() (string, error) {
	if err := e.ctx.Err(); err != nil {
		return "", err
	}
	line := editLine{historyIndex: len(e.history)}
	prompt := "gbase> "
	if e.continuation {
		prompt = "   ...> "
	}
	e.continuation = true
	if err := e.redraw(prompt, &line); err != nil {
		return "", err
	}
	escape := ""
	for {
		if err := e.ctx.Err(); err != nil {
			return "", err
		}
		r, _, err := e.input.ReadRune()
		if err != nil {
			if err == io.EOF {
				e.eof = true
				if len(line.text) > 0 {
					return e.accept(&line)
				}
			}
			return "", err
		}
		// Parse both CSI (ESC [) and application-mode (ESC O) arrow sequences.
		// Unknown sequences are consumed, never inserted into SQL or printed as text.
		if escape != "" {
			if escape == "\x1b" {
				if r == '[' || r == 'O' {
					escape += string(r)
					continue
				}
				escape = ""
			} else {
				if len(escape) <= 32 {
					escape += string(r)
				}
				if r >= 0x40 && r <= 0x7e {
					if len(escape) <= 32 {
						e.escape(escape, &line)
					}
					escape = ""
					if err := e.redraw(prompt, &line); err != nil {
						return "", err
					}
				}
				continue
			}
		}
		switch r {
		case '\x1b':
			escape = "\x1b"
			continue
		case '\r', '\n':
			return e.accept(&line)
		case '\x04':
			if len(line.text) == 0 {
				e.eof = true
				_, err := fmt.Fprint(e.output, "\n")
				if err != nil {
					return "", err
				}
				return "", io.EOF
			}
			line.delete()
		case '\x7f', '\b':
			line.backspace()
		case '\x01':
			line.cursor = 0
		case '\x05':
			line.cursor = len(line.text)
		case '\x15':
			line.text = nil
			line.cursor = 0
		case '\x0b':
			line.text = line.text[:line.cursor]
		case '\x0c':
			if _, err := fmt.Fprint(e.output, clearScreen); err != nil {
				return "", err
			}
		case '\t':
			for range 5 {
				line.insert(' ')
			}
		default:
			if unicode.IsControl(r) {
				continue
			}
			line.insert(r)
		}
		if err := e.redraw(prompt, &line); err != nil {
			return "", err
		}
	}
}

func (e *lineEditor) accept(line *editLine) (string, error) {
	text := string(line.text)
	e.remember(text)
	_, err := fmt.Fprint(e.output, "\n")
	return text, err
}

func (e *lineEditor) escape(sequence string, line *editLine) {
	switch sequence {
	case "\x1b[A", "\x1bOA":
		line.previous(e.history)
	case "\x1b[B", "\x1bOB":
		line.next(e.history)
	case "\x1b[C", "\x1bOC":
		if line.cursor < len(line.text) {
			line.cursor++
		}
	case "\x1b[D", "\x1bOD":
		if line.cursor > 0 {
			line.cursor--
		}
	case "\x1b[H", "\x1bOH", "\x1b[1~", "\x1b[7~":
		line.cursor = 0
	case "\x1b[F", "\x1bOF", "\x1b[4~", "\x1b[8~":
		line.cursor = len(line.text)
	case "\x1b[3~":
		line.delete()
	}
}

// A horizontal viewport keeps long SQL on one terminal row. Reserve the last
// column to avoid automatic wrapping, which would make erase-line clear only
// part of the previous display. Recheck width on every edit to adapt the viewport.
func (e *lineEditor) redraw(prompt string, line *editLine) error {
	columns := 80
	if e.columns != nil {
		columns = e.columns()
	}
	if columns < 2 {
		columns = 2
	}
	available := columns - displayWidth([]rune(prompt)) - 1
	if available < 1 {
		prompt = ""
		available = columns - 1
	}
	start := 0
	cursorWidth := displayWidth(line.text[:line.cursor])
	for cursorWidth > available && start < line.cursor {
		cursorWidth -= runeWidth(line.text[start])
		start++
	}
	end, width := start, 0
	for end < len(line.text) {
		next := runeWidth(line.text[end])
		if width+next > available {
			break
		}
		width += next
		end++
	}
	_, err := fmt.Fprintf(e.output, "\r\x1b[2K%s%s\r", prompt, string(line.text[start:end]))
	if err != nil {
		return err
	}
	if column := displayWidth([]rune(prompt)) + cursorWidth; column > 0 {
		_, err = fmt.Fprintf(e.output, "\x1b[%dC", column)
	}
	return err
}

func displayWidth(text []rune) int {
	width := 0
	for _, r := range text {
		width += runeWidth(r)
	}
	return width
}

// Count combining marks and common wide characters in terminal columns rather
// than bytes. Complex emoji clusters can vary by terminal; grapheme editing is
// deliberately outside this small, dependency-free editor.
func runeWidth(r rune) int {
	if unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) || r == '\u200d' {
		return 0
	}
	switch {
	case r >= 0x1100 && r <= 0x115f,
		r == 0x2329 || r == 0x232a,
		r >= 0x2e80 && r <= 0xa4cf && r != 0x303f,
		r >= 0xac00 && r <= 0xd7a3,
		r >= 0xf900 && r <= 0xfaff,
		r >= 0xfe10 && r <= 0xfe19,
		r >= 0xfe30 && r <= 0xfe6f,
		r >= 0xff00 && r <= 0xff60,
		r >= 0xffe0 && r <= 0xffe6,
		r >= 0x1f300 && r <= 0x1faff,
		r >= 0x20000 && r <= 0x3fffd:
		return 2
	}

	return 1
}
