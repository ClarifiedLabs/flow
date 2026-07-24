package terminal

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"unicode/utf8"
)

func normalizeForTest(t *testing.T, input []byte) []byte {
	t.Helper()
	got, err := NormalizeTranscript(bytes.NewReader(input))
	if err != nil {
		t.Fatalf("NormalizeTranscript: %v", err)
	}
	return got
}

func TestNormalizeTranscriptPreservesPlainUTF8ByteForByte(t *testing.T) {
	tests := []string{
		"",
		"plain output",
		"tabs\tstay\tintact\n",
		"windows\r\nline endings\r\n",
		"UTF-8: café 世界 e\u0301 👩‍💻\n",
		"trailing blank lines\n\n",
	}
	for _, input := range tests {
		t.Run(fmt.Sprintf("%q", input), func(t *testing.T) {
			got := normalizeForTest(t, []byte(input))
			if string(got) != input {
				t.Fatalf("output = %q, want exact input %q", got, input)
			}
		})
	}
}

func TestNormalizeTranscriptPlainFastPathReturnsIndependentBytes(t *testing.T) {
	input := []byte("plain text\n")
	got := normalizeForTest(t, input)
	input[0] = 'X'
	if string(got) != "plain text\n" {
		t.Fatalf("output shared input backing storage: %q", got)
	}
}

func TestNormalizeTranscriptReplaysCarriageReturnAndEraseLine(t *testing.T) {
	input := "starting work\rfinished\x1b[K\n"
	got := normalizeForTest(t, []byte(input))
	if string(got) != "finished\n" {
		t.Fatalf("output = %q, want final redraw", got)
	}
}

func TestNormalizeTranscriptCollapsesRepresentativeHarnessRedraw(t *testing.T) {
	input := "\x1b[?1049h" + // Alternate screen: flatten rather than discard.
		"\x1b[?25l\x1b(B\x1b[2J\x1b[H" + // DEC setup and clear/home.
		"[turn: 1] thinking" +
		"\r\x1b[2K[turn: 2] thinking" +
		"\r\x1b[2Kassistant: finished\n" +
		"\x1b[2 q\x1b[0m\x1b[?25h\x1b[?1049l\x1b>" // Teardown.
	got := normalizeForTest(t, []byte(input))
	const want = "assistant: finished\n"
	if string(got) != want {
		t.Fatalf("output = %q, want final visible assistant text %q", got, want)
	}
	for _, leaked := range []string{"\x1b", "[2K", "[0m", "[turn:"} {
		if bytes.Contains(got, []byte(leaked)) {
			t.Fatalf("output %q contains superseded/control text %q", got, leaked)
		}
	}
}

func TestNormalizeTranscriptReplaysCommonCursorOperations(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "short carriage return overwrite", input: "abcdef\rxy", want: "xycdef"},
		{name: "backspace", input: "abc\bX", want: "abX"},
		{name: "cursor left", input: "hello\x1b[2DXY", want: "helXY"},
		{name: "cursor right", input: "a\x1b[3Cb", want: "a   b"},
		{name: "next and previous line", input: "one\x1b[Etwo\x1b[FONE", want: "ONE\ntwo"},
		{name: "cursor up and down", input: "one\ntwo\x1b[AX\x1b[BY", want: "oneX\ntwo Y"},
		{name: "ESC index and next line", input: "a\x1bDb\x1bEc", want: "a\n b\nc"},
		{name: "canonical replay CRLF", input: "\x1b[0mone\r\ntwo\r\n", want: "one\ntwo\n"},
		{name: "absolute position", input: "top\nbottom\x1b[1;1HUP", want: "UPp\nbottom"},
		{name: "horizontal absolute", input: "abc\x1b[2GZ", want: "aZc"},
		{name: "vertical absolute", input: "top\nbottom\x1b[1dX", want: "top   X\nbottom"},
		{name: "save restore ESC", input: "abc\x1b7def\x1b8X", want: "abcXef"},
		{name: "save restore CSI", input: "abc\x1b[sdef\x1b[uX", want: "abcXef"},
		{name: "tab stops", input: "\x1b[0ma\tb", want: "a       b"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := normalizeForTest(t, []byte(test.input))
			if string(got) != test.want {
				t.Fatalf("output = %q, want %q", got, test.want)
			}
		})
	}
}

func TestNormalizeTranscriptExecutesC0ControlsInsideCSI(t *testing.T) {
	input := "abc\x1b[\rKX"
	got := normalizeForTest(t, []byte(input))
	if string(got) != "X" {
		t.Fatalf("output = %q, want embedded CR applied before CSI K", got)
	}
}

func TestNormalizeTranscriptReplaysEraseOperations(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "erase line after", input: "abcdef\x1b[4G\x1b[K", want: "abc"},
		{name: "erase line before", input: "abcdef\x1b[4G\x1b[1K", want: "    ef"},
		{name: "erase whole line", input: "abcdef\x1b[2KX", want: "      X"},
		{name: "erase characters", input: "abcde\x1b[4D\x1b[2X", want: "a  de"},
		{name: "erase display", input: "one\ntwo\x1b[1;2H\x1b[JX", want: "oX\n"},
		{name: "erase display before", input: "one\ntwo\x1b[1JX", want: "\n   X"},
		{name: "clear display", input: "old\x1b[2J\x1b[Hnew", want: "new"},
		{name: "erase scrollback no-op", input: "keep\x1b[3J", want: "keep"},
		{name: "erase final column", input: "\x1b[4096GX\x1b[K", want: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := normalizeForTest(t, []byte(test.input))
			if string(got) != test.want {
				t.Fatalf("output = %q, want %q", got, test.want)
			}
		})
	}
}

func TestNormalizeTranscriptReplaysInsertDeleteAndScrollOperations(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "insert characters", input: "abc\x1b[2D\x1b[@X", want: "aXbc"},
		{name: "delete characters", input: "abcde\x1b[4D\x1b[2P", want: "ade"},
		{name: "insert line", input: "one\ntwo\x1b[1;1H\x1b[Lzero", want: "zero\none\ntwo"},
		{name: "delete line", input: "one\ntwo\nthree\x1b[2;1H\x1b[M", want: "one\nthree\n"},
		{name: "scroll up", input: "one\ntwo\x1b[S", want: "one\ntwo\n"},
		{name: "scroll down", input: "one\ntwo\x1b[T", want: "\none"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := normalizeForTest(t, []byte(test.input))
			if string(got) != test.want {
				t.Fatalf("output = %q, want %q", got, test.want)
			}
		})
	}
}

func TestNormalizeTranscriptConsumesStylesAndControlStrings(t *testing.T) {
	input := "start" +
		"\x1b[1;38;5;200mred\x1b[0m" +
		"\x1b]0;window title\a" +
		"\x1b]8;;https://example.com\x1b\\link\x1b]8;;\x1b\\" +
		"\x1b]52;c;clipboard-secret\a" +
		"\x1bPdevice payload\x1b\\" +
		"\x1bXsos payload\x1b\\" +
		"\x1b^pm payload\x1b\\" +
		"\x1b_apc payload\x1b\\" +
		"end"
	got := normalizeForTest(t, []byte(input))
	if string(got) != "startredlinkend" {
		t.Fatalf("output = %q, want control payloads removed", got)
	}
}

func TestNormalizeTranscriptConsumesRawAndUTF8C1Controls(t *testing.T) {
	utf8Input := "\u009b31mred\u009b0m" +
		"\u009d0;title\u009c" +
		"\u0090dcs\u009c\u0098sos\u009c\u009epm\u009c\u009fapc\u009c!"
	if got := string(normalizeForTest(t, []byte(utf8Input))); got != "red!" {
		t.Fatalf("UTF-8 C1 output = %q, want %q", got, "red!")
	}

	rawInput := append([]byte{0x9b}, []byte("31mred")...)
	rawInput = append(rawInput, 0x9b)
	rawInput = append(rawInput, []byte("0m")...)
	rawInput = append(rawInput, 0x9d)
	rawInput = append(rawInput, []byte("0;title")...)
	rawInput = append(rawInput, 0x9c)
	for _, introducer := range []byte{0x90, 0x98, 0x9e, 0x9f} {
		rawInput = append(rawInput, introducer)
		rawInput = append(rawInput, []byte("secret")...)
		rawInput = append(rawInput, 0x9c)
	}
	rawInput = append(rawInput, '!')
	if got := string(normalizeForTest(t, rawInput)); got != "red!" {
		t.Fatalf("raw C1 output = %q, want %q", got, "red!")
	}
}

func TestNormalizeTranscriptDropsDanglingAndMalformedControlTraffic(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "dangling ESC", input: "ok\x1b", want: "ok"},
		{name: "partial CSI", input: "ok\x1b[123", want: "ok"},
		{name: "partial ESC intermediate", input: "ok\x1b(", want: "ok"},
		{name: "partial CSI intermediate", input: "ok\x1b[1 ", want: "ok"},
		{name: "unterminated OSC", input: "ok\x1b]0;secret", want: "ok"},
		{name: "unterminated DCS", input: "ok\x1bPsecret", want: "ok"},
		{name: "unterminated SOS", input: "ok\x1bXsecret", want: "ok"},
		{name: "unterminated PM", input: "ok\x1b^secret", want: "ok"},
		{name: "unterminated APC", input: "ok\x1b_secret", want: "ok"},
		{name: "OSC ESC not ST", input: "ok\x1b]secret\x1bxstill secret\adone", want: "okdone"},
		{name: "unknown CSI", input: "ok\x1b[1;2;3zdone", want: "okdone"},
		{name: "excess CSI parameters", input: "ok\x1b[1;2;3;4;5;6;7;8;9;10;11;12;13;14;15;16;17mdone", want: "okdone"},
		{name: "unsupported ESC", input: "ok\x1b(Bdone", want: "okdone"},
		{name: "reverse index", input: "one\ntwo\x1b[H\x1bMzero", want: "zero\none\ntwo"},
		{name: "terminal reset", input: "old\nscreen\x1bcnew", want: "new"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := normalizeForTest(t, []byte(test.input))
			if string(got) != test.want {
				t.Fatalf("output = %q, want %q", got, test.want)
			}
		})
	}
}

func TestNormalizeTranscriptDiscardsLongControlStringWithoutBufferingPayload(t *testing.T) {
	input := "ok\x1b]52;c;" + strings.Repeat("secret", 100_000) + "\aafter"
	got := normalizeForTest(t, []byte(input))
	if string(got) != "okafter" {
		t.Fatalf("output = %q, want long OSC payload discarded", got)
	}
}

func TestNormalizeTranscriptKeepsAlternateScreenText(t *testing.T) {
	input := "before\n\x1b[?1049hinside\n\x1b[?1049lafter"
	got := normalizeForTest(t, []byte(input))
	if string(got) != "before\ninside\nafter" {
		t.Fatalf("output = %q, want alternate-screen text flattened", got)
	}
}

func TestNormalizeTranscriptRetainsRowsAfterScreenScroll(t *testing.T) {
	var input strings.Builder
	var want strings.Builder
	input.WriteString("\x1b[0m") // Force replay instead of the plain fast path.
	for i := 0; i < 300; i++ {
		line := fmt.Sprintf("line %03d\n", i)
		input.WriteString(line)
		want.WriteString(line)
	}
	got := normalizeForTest(t, []byte(input.String()))
	if string(got) != want.String() {
		t.Fatalf("scrolled output mismatch: got %d bytes, want %d", len(got), want.Len())
	}
}

func TestNormalizeTranscriptDoesNotAutoWrap(t *testing.T) {
	input := "\x1b[0m" + strings.Repeat("x", transcriptColumns+100)
	got := normalizeForTest(t, []byte(input))
	if len(got) != transcriptColumns || strings.Trim(string(got), "x") != "" {
		t.Fatalf("output length/content = %d/%q, want one clamped %d-column row", len(got), got[:min(len(got), 16)], transcriptColumns)
	}
}

func TestNormalizeTranscriptPreservesCombiningMarksDuringReplay(t *testing.T) {
	input := "\x1b[0me\u0301!"
	got := normalizeForTest(t, []byte(input))
	if string(got) != "e\u0301!" {
		t.Fatalf("output = %q, want combining sequence preserved", got)
	}
}

func TestNormalizeTranscriptReplacesInvalidUTF8AndRemovesControls(t *testing.T) {
	input := []byte{'a', 0xff, 'b', 0, '\n', 'c'}
	got := normalizeForTest(t, input)
	if !utf8.Valid(got) {
		t.Fatalf("output is invalid UTF-8: %x", got)
	}
	if string(got) != "a�b\nc" {
		t.Fatalf("output = %q, want replacement rune and no NUL", got)
	}
}

func TestNormalizeTranscriptAttachesCombiningMarkAtLastColumn(t *testing.T) {
	input := "\x1b[4096Gx\u0301"
	got := normalizeForTest(t, []byte(input))
	if !bytes.HasSuffix(got, []byte("x\u0301")) {
		t.Fatalf("output does not retain final-column combining mark: suffix %q", got[max(0, len(got)-16):])
	}
}

func TestNormalizeTranscriptClampsHugeCursorCoordinates(t *testing.T) {
	input := "\x1b[999999999999;999999999999HX"
	got := normalizeForTest(t, []byte(input))
	if !bytes.HasSuffix(got, []byte("X")) {
		t.Fatalf("output does not retain text at clamped cursor: suffix %q", got[max(0, len(got)-16):])
	}
	if len(got) > transcriptRows+transcriptColumns {
		t.Fatalf("output length = %d, cursor expansion was not bounded", len(got))
	}
}

func TestNormalizeTranscriptBoundsRepetitiveScreenEdits(t *testing.T) {
	fullRow := strings.Repeat("x", transcriptColumns)
	insertions := strings.Repeat("\r\x1b[@", transcriptWorkLimit/(2*transcriptColumns)+transcriptColumns)
	got := normalizeForTest(t, []byte(fullRow+insertions))
	if !bytes.Contains(got, []byte("x")) {
		t.Fatalf("work-limit output did not retain the partially rendered screen")
	}
	if !bytes.HasSuffix(got, []byte(transcriptTruncationMarker)) {
		t.Fatalf("output lacks work-limit truncation marker")
	}
	if len(got) > transcriptOutputLimit {
		t.Fatalf("output length = %d, exceeds %d", len(got), transcriptOutputLimit)
	}
}

func TestNormalizeTranscriptHonorsInputAndOutputLimits(t *testing.T) {
	t.Run("input", func(t *testing.T) {
		input := bytes.Repeat([]byte("x\n"), transcriptInputLimit/2+1)
		got := normalizeForTest(t, input)
		if len(got) > transcriptOutputLimit {
			t.Fatalf("output length = %d, exceeds %d", len(got), transcriptOutputLimit)
		}
		if !bytes.Contains(got, []byte(transcriptTruncationMarker)) {
			t.Fatalf("output lacks truncation marker")
		}
	})

	t.Run("truncated UTF-8 at input boundary", func(t *testing.T) {
		input := bytes.Repeat([]byte{'a'}, transcriptInputLimit-1)
		input = append(input, 0xe2, 0x82)
		got := normalizeForTest(t, input)
		if !utf8.Valid(got) {
			t.Fatalf("output is invalid UTF-8")
		}
		if !bytes.HasSuffix(got, []byte(transcriptTruncationMarker)) {
			t.Fatalf("output lacks final truncation marker")
		}
	})

	t.Run("expanded output", func(t *testing.T) {
		input := strings.Repeat("\x1b[4096Cx\n", 3000)
		got := normalizeForTest(t, []byte(input))
		if len(got) > transcriptOutputLimit {
			t.Fatalf("output length = %d, exceeds %d", len(got), transcriptOutputLimit)
		}
		if !bytes.HasSuffix(got, []byte(transcriptTruncationMarker)) {
			t.Fatalf("output lacks final truncation marker")
		}
		if !utf8.Valid(got) {
			t.Fatalf("truncated output is invalid UTF-8")
		}
	})
}

func TestNormalizeTranscriptHandlesSequencesSplitAcrossReads(t *testing.T) {
	reader := &oneByteReader{data: []byte("wait\rready\x1b[K\n\x1b]0;title\a")}
	got, err := NormalizeTranscript(reader)
	if err != nil {
		t.Fatalf("NormalizeTranscript: %v", err)
	}
	if string(got) != "ready\n" {
		t.Fatalf("output = %q, want split controls replayed", got)
	}
}

func TestNormalizeTranscriptReturnsReadErrors(t *testing.T) {
	wantErr := errors.New("read failed")
	got, err := NormalizeTranscript(&errorReader{err: wantErr})
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want %v", err, wantErr)
	}
	if got != nil {
		t.Fatalf("output = %q, want nil after read error", got)
	}
}

func FuzzNormalizeTranscript(f *testing.F) {
	for _, seed := range [][]byte{
		[]byte("plain\ntext\n"),
		[]byte("progress 1%\rprogress 100%\x1b[K\n"),
		[]byte("\x1b[?1049hconversation\x1b[?1049l"),
		{0xff, 0x9b, '3', '1', 'm', 'x', 0x9c},
		[]byte("\x1b]unterminated"),
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, input []byte) {
		got, err := NormalizeTranscript(bytes.NewReader(input))
		if err != nil {
			t.Fatalf("NormalizeTranscript: %v", err)
		}
		if len(got) > transcriptOutputLimit {
			t.Fatalf("output length = %d, exceeds %d", len(got), transcriptOutputLimit)
		}
		if !utf8.Valid(got) {
			t.Fatalf("output is invalid UTF-8: %x", got)
		}
		if err := validateRenderedControls(got); err != nil {
			t.Fatal(err)
		}

		again, err := NormalizeTranscript(bytes.NewReader(got))
		if err != nil {
			t.Fatalf("second NormalizeTranscript: %v", err)
		}
		if !bytes.Equal(again, got) {
			t.Fatalf("normalization is not idempotent: first %q, second %q", got, again)
		}
	})
}

func validateRenderedControls(data []byte) error {
	for i := 0; i < len(data); {
		b := data[i]
		if b < utf8.RuneSelf {
			switch b {
			case '\t', '\n':
				i++
				continue
			case '\r':
				if i+1 < len(data) && data[i+1] == '\n' {
					i++
					continue
				}
			}
			if b < ' ' || b == 0x7f {
				return fmt.Errorf("output contains control byte %#x at %d", b, i)
			}
			i++
			continue
		}
		r, size := utf8.DecodeRune(data[i:])
		if r >= 0x80 && r <= 0x9f {
			return fmt.Errorf("output contains C1 control %U at %d", r, i)
		}
		i += size
	}
	return nil
}

type oneByteReader struct {
	data []byte
}

func (r *oneByteReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	p[0] = r.data[0]
	r.data = r.data[1:]
	return 1, nil
}

type errorReader struct {
	err  error
	read bool
}

func (r *errorReader) Read(p []byte) (int, error) {
	if !r.read {
		r.read = true
		copy(p, "partial")
		return len("partial"), nil
	}
	return 0, r.err
}
