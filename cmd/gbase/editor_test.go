package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
)

func editorLines(t *testing.T, input string) ([]string, *lineEditor, string) {
	t.Helper()
	var output bytes.Buffer
	editor := newLineEditor(context.Background(), strings.NewReader(input), &output, func() int { return 80 })
	reader := bufio.NewReader(editor)
	var lines []string
	for {
		line, err := reader.ReadString('\n')
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, strings.TrimSuffix(line, "\n"))
	}
	return lines, editor, output.String()
}

func TestEditorHistoryAndDraft(t *testing.T) {
	input := "SELECT 1;\nSELECT 2;\nSELECT 3;\x1b[A\x1b[A\x1b[A\x1b[B\x1b[B\x1b[B\n\x1bOA\n"
	got, editor, _ := editorLines(t, input)
	want := []string{"SELECT 1;", "SELECT 2;", "SELECT 3;", "SELECT 3;"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v want %#v", got, want)
	}
	if !reflect.DeepEqual(editor.history, want[:3]) {
		t.Fatalf("history %#v", editor.history)
	}
}

func TestEditorEditRecalledLine(t *testing.T) {
	got, editor, _ := editorLines(t, "SELECT 1;\n\x1b[A\x1b[D\x7f2\n\x1b[A\x1b[A\n")
	want := []string{"SELECT 1;", "SELECT 2;", "SELECT 1;"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v", got)
	}
	if editor.history[0] != "SELECT 1;" {
		t.Fatal("edited original history entry")
	}
}

func TestEditorKeys(t *testing.T) {
	tests := []struct{ input, want string }{
		{"abc\x1b[D\x1b[DZ\n", "aZbc"},
		{"abc\x1b[H\x1b[3~\x1b[F\bD\n", "bD"},
		{"abc\x01Z\x05Q\n", "ZabcQ"},
		{"discard\x15keep\n", "keep"},
		{"abcdef\x1b[D\x1b[D\x0b\n", "abcd"},
		{"ab\x1b[H\x04\n", "b"},
		{"é界\x1b[D\bA\n", "A界"},
		{"a\tB\n", "a    B"},
		{"a\x1b[99~b\n", "ab"},
		{"a\x1b[" + strings.Repeat("1;", 100) + "~b\n", "ab"},
		{"a\x1bb\n", "ab"},
		{"\x1b[A\x1b[Bsafe\n", "safe"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got, _, _ := editorLines(t, tt.input)
			if !reflect.DeepEqual(got, []string{tt.want}) {
				t.Fatalf("%q: %#v", tt.input, got)
			}
		})
	}
}

func TestEditorEOFAndSmallReads(t *testing.T) {
	for _, input := range []string{"hello\n\x04", "hello"} {
		editor := newLineEditor(context.Background(), strings.NewReader(input), io.Discard, nil)
		var result strings.Builder
		byteBuffer := make([]byte, 1)
		for {
			n, err := editor.Read(byteBuffer)
			result.Write(byteBuffer[:n])
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
		}
		if result.String() != "hello\n" {
			t.Fatal(result.String())
		}
		if n, err := editor.Read(nil); n != 0 || err != nil {
			t.Fatalf("zero-length read: %d %v", n, err)
		}
	}
}

func TestEditorHistoryLimit(t *testing.T) {
	editor := newLineEditor(context.Background(), strings.NewReader(""), io.Discard, nil)
	editor.remember(" ")
	for i := 0; i < historyLimit+10; i++ {
		editor.remember(strings.Repeat("x", i+1))
	}
	if len(editor.history) != historyLimit || len(editor.history[0]) != 11 {
		t.Fatal("history not bounded")
	}
	editor.remember(editor.history[len(editor.history)-1])
	if len(editor.history) != historyLimit {
		t.Fatal("consecutive duplicate retained")
	}
}

func TestEditorFeedsSQLSplitter(t *testing.T) {
	var output bytes.Buffer
	editor := newLineEditor(context.Background(), strings.NewReader("SELECT\n'a;''b'; SELECT 2;\n.check\n\x04"), &output, nil)
	splitter := splitter{r: bufio.NewReader(editor)}
	var got []string
	for {
		statement, err := splitter.next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, statement)
	}
	want := []string{"SELECT\n'a;''b'", "SELECT 2", ".check"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v", got)
	}
	if !strings.Contains(output.String(), "...> ") {
		t.Fatal("missing continuation prompt")
	}
}

func TestEditorViewport(t *testing.T) {
	var output bytes.Buffer
	editor := newLineEditor(context.Background(), strings.NewReader(""), &output, func() int { return 16 })
	line := editLine{text: []rune("SELECT abcdefghijklmnop;"), cursor: 24}
	if err := editor.redraw("gbase> ", &line); err != nil {
		t.Fatal(err)
	}
	if output.String() != "\r\x1b[2Kgbase> jklmnop;\r\x1b[15C" {
		t.Fatalf("viewport %q", output.String())
	}
	if displayWidth([]rune("é界e\u0301")) != 4 {
		t.Fatal("incorrect display width")
	}
}

func TestEditorCancellationAndOutputError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	editor := newLineEditor(ctx, strings.NewReader("ignored\n"), io.Discard, nil)
	if _, err := editor.Read(make([]byte, 1)); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	editor = newLineEditor(context.Background(), strings.NewReader("ignored\n"), failingOutput{}, nil)
	if _, err := editor.Read(make([]byte, 1)); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal(err)
	}
}

type failingOutput struct{}

func (failingOutput) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
