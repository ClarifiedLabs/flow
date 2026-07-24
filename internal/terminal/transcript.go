package terminal

// The parser state dispatch in this file is adapted from ansi2html.c in
// kilobyte/colorized-logs. See ../../THIRD_PARTY_NOTICES.md and the upstream
// source at https://github.com/kilobyte/colorized-logs/blob/2fb2ea5db6c830b8426a2e7084e0ad5af83f1070/ansi2html.c.
// Flow's mutable text screen, terminal operations, and plain-text output are
// purpose-built; no upstream HTML, styling, color, or CLI code is used here.

import (
	"io"
	"unicode"
	"unicode/utf8"
)

const (
	// Transcripts are already capped by the coordinator. Enforcing the same
	// limit here also bounds manually replaced or corrupt files.
	transcriptInputLimit = 10 << 20
	// The retained screen is deliberately fixed because tmux pipe-pane does not
	// include dimensions or resize events. Graphic text is never auto-wrapped.
	transcriptRows    = 256
	transcriptColumns = 4096
	transcriptCSIMax  = 16
	// A small continuation bound prevents a hostile stream of combining marks
	// from growing one cell without limit. Ordinary valid UTF-8 avoids replay.
	maxCellContinuations  = 16
	maxCSIValue           = 1 << 30
	transcriptOutputLimit = 10 << 20
	// Some bounded screen edits are O(rows) or O(columns). Bound their total
	// work as well as input/output so repetitive non-printing controls cannot
	// consume disproportionate CPU.
	transcriptWorkLimit = 16 << 20
)

const transcriptTruncationMarker = "\n[transcript rendering truncated]\n"

// NormalizeTranscript replays common ANSI/VT terminal operations into bounded,
// terminal-rendered plain text. Plain valid UTF-8 logs without terminal control
// traffic are returned byte-for-byte, including CRLF and trailing newlines.
func NormalizeTranscript(src io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(src, transcriptInputLimit+1))
	if err != nil {
		return nil, err
	}
	inputTruncated := len(data) > transcriptInputLimit
	if inputTruncated {
		data = data[:transcriptInputLimit]
	}

	if !inputTruncated && isPlainTranscript(data) {
		return append([]byte(nil), data...), nil
	}

	collector := outputCollector{}
	parser := newTranscriptParser(&collector)
	parser.parse(data)
	if !collector.truncated {
		parser.screen.render(&collector)
	}
	if inputTruncated || parser.screen.workExhausted {
		collector.markTruncated()
	}
	return collector.buf, nil
}

func isPlainTranscript(data []byte) bool {
	if !utf8.Valid(data) {
		return false
	}
	for i := 0; i < len(data); {
		b := data[i]
		if b < utf8.RuneSelf {
			switch b {
			case '\t', '\n':
				i++
				continue
			case '\r':
				if i+1 >= len(data) || data[i+1] != '\n' {
					return false
				}
				i++
				continue
			}
			if b < ' ' || b == 0x7f {
				return false
			}
			i++
			continue
		}

		r, size := utf8.DecodeRune(data[i:])
		if r >= 0x80 && r <= 0x9f {
			return false
		}
		i += size
	}
	return true
}

type parserState uint8

const (
	stateGround parserState = iota
	stateEscape
	stateEscapeIntermediate
	stateCSI
	stateCSIIntermediate
	stateOSC
	stateDCS
	stateSOS
	statePM
	stateAPC
	stateStringEscape
)

type transcriptParser struct {
	state       parserState
	stringState parserState
	screen      *textScreen

	params       [transcriptCSIMax]int
	paramIndex   int
	private      bool
	tooMany      bool
	malformedCSI bool
}

func newTranscriptParser(out *outputCollector) *transcriptParser {
	return &transcriptParser{
		state:  stateGround,
		screen: newTextScreen(out),
	}
}

func (p *transcriptParser) parse(data []byte) {
	for i := 0; i < len(data) && !p.screen.out.truncated && !p.screen.workExhausted; {
		b := data[i]
		if b < utf8.RuneSelf {
			p.feedByte(b)
			i++
			continue
		}
		// Raw C1 bytes are controls. A C1-looking continuation byte inside a
		// valid UTF-8 rune is consumed by DecodeRune below instead.
		if b >= 0x80 && b <= 0x9f {
			p.feedC1(b)
			i++
			continue
		}

		r, size := utf8.DecodeRune(data[i:])
		if r == utf8.RuneError && size == 1 {
			p.feedRune(utf8.RuneError)
			i++
			continue
		}
		if r >= 0x80 && r <= 0x9f {
			p.feedC1(byte(r))
		} else {
			p.feedRune(r)
		}
		i += size
	}
}

func (p *transcriptParser) feedRune(r rune) {
	switch p.state {
	case stateGround:
		p.screen.writeRune(r)
	case stateStringEscape:
		p.state = p.stringState
	case stateEscape, stateEscapeIntermediate:
		// A non-ECMA byte completes an unsupported escape without becoming
		// visible text.
		p.state = stateGround
	case stateCSI, stateCSIIntermediate:
		p.malformedCSI = true
	// All control-string payload is discarded incrementally.
	case stateOSC, stateDCS, stateSOS, statePM, stateAPC:
	}
}

func (p *transcriptParser) feedByte(b byte) {
	if isStringState(p.state) || p.state == stateStringEscape {
		p.feedStringByte(b)
		return
	}

	if b == 0x1b {
		p.state = stateEscape
		return
	}
	if b == 0x18 || b == 0x1a { // CAN and SUB cancel an in-progress sequence.
		p.state = stateGround
		return
	}
	if p.state != stateGround && (b < 0x20 || b == 0x7f) {
		// ECMA-48 executes embedded C0 controls while retaining the current
		// ESC/CSI state. Control-string payload is handled above and discarded.
		p.feedGroundByte(b)
		return
	}

	switch p.state {
	case stateGround:
		p.feedGroundByte(b)
	case stateEscape:
		p.feedEscapeByte(b)
	case stateEscapeIntermediate:
		p.feedEscapeIntermediateByte(b)
	case stateCSI:
		p.feedCSIByte(b)
	case stateCSIIntermediate:
		p.feedCSIIntermediateByte(b)
	}
}

func (p *transcriptParser) feedGroundByte(b byte) {
	switch b {
	case '\b':
		p.screen.backspace()
	case '\t':
		p.screen.tab()
	case '\n', '\v', '\f':
		p.screen.index(true)
	case '\r':
		p.screen.carriageReturn()
	default:
		if b >= ' ' && b < 0x7f {
			p.screen.writeRune(rune(b))
		}
		// BEL, NUL, DEL, and other C0 controls are non-printing.
	}
}

func (p *transcriptParser) feedEscapeByte(b byte) {
	switch b {
	case '[':
		p.beginCSI()
	case ']':
		p.state = stateOSC
	case 'P':
		p.state = stateDCS
	case 'X':
		p.state = stateSOS
	case '^':
		p.state = statePM
	case '_':
		p.state = stateAPC
	case 'D':
		p.screen.index(false)
		p.state = stateGround
	case 'E':
		p.screen.index(true)
		p.state = stateGround
	case 'M':
		p.screen.reverseIndex()
		p.state = stateGround
	case '7':
		p.screen.saveCursor()
		p.state = stateGround
	case '8':
		p.screen.restoreCursor()
		p.state = stateGround
	case 'c':
		p.screen.reset()
		p.state = stateGround
	default:
		switch {
		case b >= 0x20 && b <= 0x2f:
			p.state = stateEscapeIntermediate
		case b >= 0x30 && b <= 0x7e:
			// Unsupported complete ESC operation.
			p.state = stateGround
		case b < 0x20 || b == 0x7f:
			// Embedded C0 controls do not terminate the escape.
		default:
			p.state = stateGround
		}
	}
}

func (p *transcriptParser) feedEscapeIntermediateByte(b byte) {
	switch {
	case b >= 0x20 && b <= 0x2f:
		// Continue collecting intermediates without storing them.
	case b >= 0x30 && b <= 0x7e:
		p.state = stateGround
	case b < 0x20 || b == 0x7f:
		// Consume embedded controls.
	default:
		p.state = stateGround
	}
}

func (p *transcriptParser) beginCSI() {
	p.state = stateCSI
	p.params = [transcriptCSIMax]int{}
	p.paramIndex = 0
	p.private = false
	p.tooMany = false
	p.malformedCSI = false
}

func (p *transcriptParser) feedCSIByte(b byte) {
	switch {
	case b >= '0' && b <= '9':
		if p.paramIndex < len(p.params) {
			value := p.params[p.paramIndex]
			digit := int(b - '0')
			if value > (maxCSIValue-digit)/10 {
				value = maxCSIValue
			} else {
				value = value*10 + digit
			}
			p.params[p.paramIndex] = value
		}
	case b == ';' || b == ':':
		if p.paramIndex+1 < len(p.params) {
			p.paramIndex++
		} else {
			p.tooMany = true
		}
	case b >= 0x3c && b <= 0x3f:
		p.private = true
	case b >= 0x30 && b <= 0x3f:
		p.malformedCSI = true
	case b >= 0x20 && b <= 0x2f:
		p.state = stateCSIIntermediate
	case b >= 0x40 && b <= 0x7e:
		p.executeCSI(b)
		p.state = stateGround
	case b < 0x20 || b == 0x7f:
		// Consume embedded C0 controls without exposing them.
	default:
		p.malformedCSI = true
	}
}

func (p *transcriptParser) feedCSIIntermediateByte(b byte) {
	switch {
	case b >= 0x20 && b <= 0x2f:
	case b >= 0x40 && b <= 0x7e:
		// Commands with intermediates (for example cursor style) are consumed
		// as no-ops.
		p.state = stateGround
	case b < 0x20 || b == 0x7f:
	default:
		p.malformedCSI = true
	}
}

func (p *transcriptParser) executeCSI(final byte) {
	// Private modes include alternate-screen switching. They are deliberately
	// no-ops so printable text inside ?47/?1047/?1049 remains in the transcript.
	if p.private || p.malformedCSI {
		return
	}

	n := p.paramOrOne(0)
	switch final {
	case 'A':
		p.screen.moveRow(-bounded(n, transcriptRows))
	case 'B':
		p.screen.moveRow(bounded(n, transcriptRows))
	case 'C':
		p.screen.moveCol(bounded(n, transcriptColumns))
	case 'D':
		p.screen.moveCol(-bounded(n, transcriptColumns))
	case 'E':
		p.screen.moveRow(bounded(n, transcriptRows))
		p.screen.setCol(0)
	case 'F':
		p.screen.moveRow(-bounded(n, transcriptRows))
		p.screen.setCol(0)
	case 'G', '`':
		p.screen.setCol(p.paramOrOne(0) - 1)
	case 'd':
		p.screen.setRow(p.paramOrOne(0) - 1)
	case 'H', 'f':
		p.screen.setRow(p.paramOrOne(0) - 1)
		p.screen.setCol(p.paramOrOne(1) - 1)
	case 'J':
		p.screen.eraseDisplay(p.param(0))
	case 'K':
		p.screen.eraseLine(p.param(0))
	case 'X':
		p.screen.eraseCharacters(n)
	case '@':
		p.screen.insertCharacters(n)
	case 'P':
		p.screen.deleteCharacters(n)
	case 'L':
		p.screen.insertLines(n)
	case 'M':
		p.screen.deleteLines(n)
	case 'S':
		p.screen.scrollUp(n)
	case 'T':
		p.screen.scrollDown(n)
	case 's':
		p.screen.saveCursor()
	case 'u':
		p.screen.restoreCursor()
		// SGR and all unsupported operations are consumed no-ops.
	}
}

func (p *transcriptParser) param(index int) int {
	if index < 0 || index > p.paramIndex || index >= len(p.params) {
		return 0
	}
	return p.params[index]
}

func (p *transcriptParser) paramOrOne(index int) int {
	value := p.param(index)
	if value == 0 {
		return 1
	}
	return value
}

func (p *transcriptParser) feedStringByte(b byte) {
	if p.state == stateStringEscape {
		if b == '\\' {
			p.state = stateGround
			return
		}
		p.state = p.stringState
	}
	if b == 0x1b {
		p.stringState = p.state
		p.state = stateStringEscape
		return
	}
	if p.state == stateOSC && b == '\a' {
		p.state = stateGround
	}
}

func (p *transcriptParser) feedC1(control byte) {
	if isStringState(p.state) {
		if control == 0x9c { // ST
			p.state = stateGround
		}
		return
	}
	if p.state == stateStringEscape {
		if control == 0x9c {
			p.state = stateGround
		} else {
			p.state = p.stringState
		}
		return
	}

	switch control {
	case 0x84: // IND
		p.screen.index(false)
		p.state = stateGround
	case 0x85: // NEL
		p.screen.index(true)
		p.state = stateGround
	case 0x8d: // RI
		p.screen.reverseIndex()
		p.state = stateGround
	case 0x90:
		p.state = stateDCS
	case 0x98:
		p.state = stateSOS
	case 0x9b:
		p.beginCSI()
	case 0x9c:
		p.state = stateGround
	case 0x9d:
		p.state = stateOSC
	case 0x9e:
		p.state = statePM
	case 0x9f:
		p.state = stateAPC
	default:
		// Other C1 controls are non-printing. If they interrupt a sequence,
		// abandon it rather than exposing a partial payload.
		p.state = stateGround
	}
}

func isStringState(state parserState) bool {
	return state == stateOSC || state == stateDCS || state == stateSOS || state == statePM || state == stateAPC
}

type screenRow struct {
	cells []string
}

func (r *screenRow) set(col int, text string) {
	if col < 0 || col >= transcriptColumns {
		return
	}
	if len(r.cells) <= col {
		r.cells = append(r.cells, make([]string, col+1-len(r.cells))...)
	}
	r.cells[col] = text
}

func (r *screenRow) appendContinuation(col int, continuation rune) bool {
	if col < 0 || col >= len(r.cells) || r.cells[col] == "" {
		return false
	}
	if utf8.RuneCountInString(r.cells[col]) >= maxCellContinuations+1 {
		return true
	}
	r.cells[col] += string(continuation)
	return true
}

func (r *screenRow) erase(start int, end int) {
	start, end = boundedScreenRange(start, end)
	if start >= end || start >= len(r.cells) {
		return
	}
	if end > len(r.cells) {
		end = len(r.cells)
	}
	for i := start; i < end; i++ {
		r.cells[i] = ""
	}
	r.trim()
}

func eraseWork(row *screenRow, start int, end int) int {
	start, end = boundedScreenRange(start, end)
	if start >= end || start >= len(row.cells) {
		return 0
	}
	end = min(end, len(row.cells))
	// Clearing and trimming can each inspect the erased suffix.
	return 2 * (end - start)
}

func boundedScreenRange(start int, end int) (int, int) {
	start = max(0, min(start, transcriptColumns))
	end = max(0, min(end, transcriptColumns))
	return start, end
}

func (r *screenRow) trim() {
	end := len(r.cells)
	for end > 0 && r.cells[end-1] == "" {
		end--
	}
	r.cells = r.cells[:end]
}

func (r *screenRow) bytes() []byte {
	end := len(r.cells)
	for end > 0 && r.cells[end-1] == "" {
		end--
	}
	if end == 0 {
		return nil
	}
	result := make([]byte, 0, end)
	for _, cell := range r.cells[:end] {
		if cell == "" {
			result = append(result, ' ')
		} else {
			result = append(result, cell...)
		}
	}
	return result
}

type textScreen struct {
	rows []screenRow
	row  int
	col  int

	savedRow int
	savedCol int
	hasSaved bool

	continuationRow int
	continuationCol int
	canContinue     bool

	workRemaining int
	workExhausted bool
	out           *outputCollector
}

func newTextScreen(out *outputCollector) *textScreen {
	return &textScreen{
		rows:          []screenRow{{}},
		workRemaining: transcriptWorkLimit,
		out:           out,
	}
}

func (s *textScreen) consumeWork(units int) bool {
	if units <= s.workRemaining {
		s.workRemaining -= units
		return true
	}
	s.workRemaining = 0
	s.workExhausted = true
	return false
}

func (s *textScreen) currentRow() *screenRow {
	s.ensureRow(s.row)
	return &s.rows[s.row]
}

func (s *textScreen) writeRune(r rune) {
	row := s.currentRow()
	if unicode.Is(unicode.M, r) || unicode.Is(unicode.Cf, r) {
		if s.canContinue && s.continuationRow == s.row && row.appendContinuation(s.continuationCol, r) {
			return
		}
		if s.col > 0 && row.appendContinuation(s.col-1, r) {
			return
		}
	}
	growth := s.col + 1 - len(row.cells)
	if growth > 0 && !s.consumeWork(growth) {
		return
	}
	row.set(s.col, string(r))
	s.continuationRow = s.row
	s.continuationCol = s.col
	s.canContinue = true
	if s.col < transcriptColumns-1 {
		s.col++
	}
}

func (s *textScreen) clearContinuation() {
	s.canContinue = false
}

func (s *textScreen) carriageReturn() {
	s.clearContinuation()
	s.col = 0
}

func (s *textScreen) backspace() {
	s.clearContinuation()
	if s.col > 0 {
		s.col--
	}
}

func (s *textScreen) tab() {
	next := (s.col/8 + 1) * 8
	s.setCol(next)
}

func (s *textScreen) index(resetColumn bool) {
	s.clearContinuation()
	if resetColumn {
		s.col = 0
	}
	if s.row+1 < len(s.rows) {
		s.row++
		return
	}
	if len(s.rows) < transcriptRows {
		s.rows = append(s.rows, screenRow{})
		s.row++
		return
	}
	s.flushTopRow()
	s.row = len(s.rows) - 1
}

func (s *textScreen) reverseIndex() {
	s.clearContinuation()
	if s.row > 0 {
		s.row--
		return
	}
	if !s.consumeWork(len(s.rows) + 1) {
		return
	}
	if len(s.rows) == transcriptRows {
		copy(s.rows[1:], s.rows[:len(s.rows)-1])
		s.rows[0] = screenRow{}
		return
	}
	rows := make([]screenRow, 0, len(s.rows)+1)
	rows = append(rows, screenRow{})
	rows = append(rows, s.rows...)
	s.rows = rows
}

func (s *textScreen) moveRow(delta int) {
	s.setRow(s.row + delta)
}

func (s *textScreen) setRow(row int) {
	s.clearContinuation()
	s.row = bounded(row, transcriptRows)
	s.ensureRow(s.row)
}

func (s *textScreen) moveCol(delta int) {
	s.setCol(s.col + delta)
}

func (s *textScreen) setCol(col int) {
	s.clearContinuation()
	s.col = bounded(col, transcriptColumns)
}

func (s *textScreen) ensureRow(row int) {
	row = bounded(row, transcriptRows)
	for len(s.rows) <= row {
		s.rows = append(s.rows, screenRow{})
	}
}

func (s *textScreen) saveCursor() {
	s.savedRow = s.row
	s.savedCol = s.col
	s.hasSaved = true
}

func (s *textScreen) restoreCursor() {
	if !s.hasSaved {
		return
	}
	s.setRow(s.savedRow)
	s.setCol(s.savedCol)
}

func (s *textScreen) reset() {
	s.clearContinuation()
	s.rows = []screenRow{{}}
	s.row = 0
	s.col = 0
	s.savedRow = 0
	s.savedCol = 0
	s.hasSaved = false
}

func (s *textScreen) eraseLine(mode int) {
	s.clearContinuation()
	row := s.currentRow()
	switch mode {
	case 0:
		if s.consumeWork(eraseWork(row, s.col, transcriptColumns)) {
			row.erase(s.col, transcriptColumns)
		}
	case 1:
		if s.consumeWork(eraseWork(row, 0, s.col+1)) {
			row.erase(0, s.col+1)
		}
	case 2:
		row.cells = nil
	}
}

func (s *textScreen) eraseDisplay(mode int) {
	s.clearContinuation()
	s.ensureRow(s.row)
	switch mode {
	case 0:
		work := eraseWork(&s.rows[s.row], s.col, transcriptColumns) + len(s.rows) - s.row - 1
		if !s.consumeWork(work) {
			return
		}
		s.rows[s.row].erase(s.col, transcriptColumns)
		for i := s.row + 1; i < len(s.rows); i++ {
			s.rows[i].cells = nil
		}
	case 1:
		work := s.row + eraseWork(&s.rows[s.row], 0, s.col+1)
		if !s.consumeWork(work) {
			return
		}
		for i := 0; i < s.row; i++ {
			s.rows[i].cells = nil
		}
		s.rows[s.row].erase(0, s.col+1)
	case 2:
		if !s.consumeWork(len(s.rows)) {
			return
		}
		for i := range s.rows {
			s.rows[i].cells = nil
		}
	case 3:
		// Already-flushed history cannot and should not be retracted.
	}
}

func (s *textScreen) eraseCharacters(count int) {
	s.clearContinuation()
	count = boundedNonZero(count, transcriptColumns)
	row := s.currentRow()
	if s.consumeWork(eraseWork(row, s.col, s.col+count)) {
		row.erase(s.col, s.col+count)
	}
}

func (s *textScreen) insertCharacters(count int) {
	s.clearContinuation()
	count = boundedNonZero(count, transcriptColumns)
	row := s.currentRow()
	if s.col >= len(row.cells) {
		return
	}
	room := transcriptColumns - s.col
	if count > room {
		count = room
	}
	oldLen := len(row.cells)
	newLen := min(oldLen+count, transcriptColumns)
	if !s.consumeWork(oldLen - s.col + newLen - s.col) {
		return
	}
	row.cells = append(row.cells, make([]string, newLen-oldLen)...)
	copy(row.cells[s.col+count:], row.cells[s.col:oldLen])
	for i := s.col; i < s.col+count && i < len(row.cells); i++ {
		row.cells[i] = ""
	}
	row.trim()
}

func (s *textScreen) deleteCharacters(count int) {
	s.clearContinuation()
	count = boundedNonZero(count, transcriptColumns)
	row := s.currentRow()
	if s.col >= len(row.cells) {
		return
	}
	end := min(s.col+count, len(row.cells))
	if !s.consumeWork(len(row.cells) - s.col) {
		return
	}
	copy(row.cells[s.col:], row.cells[end:])
	row.cells = row.cells[:len(row.cells)-(end-s.col)]
	row.trim()
}

func (s *textScreen) insertLines(count int) {
	s.clearContinuation()
	count = boundedNonZero(count, transcriptRows)
	s.ensureRow(s.row)
	if count > transcriptRows-s.row {
		count = transcriptRows - s.row
	}
	if !s.consumeWork(len(s.rows) + count) {
		return
	}
	rows := make([]screenRow, 0, min(len(s.rows)+count, transcriptRows))
	rows = append(rows, s.rows[:s.row]...)
	rows = append(rows, make([]screenRow, count)...)
	rows = append(rows, s.rows[s.row:]...)
	if len(rows) > transcriptRows {
		rows = rows[:transcriptRows]
	}
	s.rows = rows
}

func (s *textScreen) deleteLines(count int) {
	s.clearContinuation()
	count = boundedNonZero(count, transcriptRows)
	s.ensureRow(s.row)
	oldLen := len(s.rows)
	end := min(s.row+count, oldLen)
	removed := end - s.row
	if !s.consumeWork(oldLen - s.row + removed) {
		return
	}
	copy(s.rows[s.row:], s.rows[end:])
	s.rows = s.rows[:oldLen-removed]
	s.rows = append(s.rows, make([]screenRow, removed)...)
}

func (s *textScreen) scrollUp(count int) {
	s.clearContinuation()
	count = min(boundedNonZero(count, transcriptRows), len(s.rows))
	for range count {
		if !s.flushTopRow() {
			return
		}
	}
}

func (s *textScreen) scrollDown(count int) {
	s.clearContinuation()
	count = min(boundedNonZero(count, transcriptRows), len(s.rows))
	if count == 0 {
		return
	}
	oldLen := len(s.rows)
	if !s.consumeWork(oldLen) {
		return
	}
	rows := make([]screenRow, 0, oldLen)
	rows = append(rows, make([]screenRow, count)...)
	rows = append(rows, s.rows[:oldLen-count]...)
	s.rows = rows
}

func (s *textScreen) flushTopRow() bool {
	if len(s.rows) == 0 {
		s.rows = []screenRow{{}}
		return true
	}
	if !s.consumeWork(len(s.rows)) {
		return false
	}
	if !s.out.append(s.rows[0].bytes()) || !s.out.append([]byte{'\n'}) {
		return false
	}
	copy(s.rows, s.rows[1:])
	s.rows[len(s.rows)-1] = screenRow{}
	return true
}

func (s *textScreen) render(out *outputCollector) {
	for i := range s.rows {
		if !out.append(s.rows[i].bytes()) {
			return
		}
		if i+1 < len(s.rows) && !out.append([]byte{'\n'}) {
			return
		}
	}
}

type outputCollector struct {
	buf       []byte
	truncated bool
}

func (c *outputCollector) append(data []byte) bool {
	if c.truncated {
		return false
	}
	if len(c.buf)+len(data) <= transcriptOutputLimit {
		c.buf = append(c.buf, data...)
		return true
	}

	limit := transcriptOutputLimit - len(transcriptTruncationMarker)
	room := limit - len(c.buf)
	if room > 0 {
		if room > len(data) {
			room = len(data)
		}
		room = validUTF8Prefix(data, room)
		c.buf = append(c.buf, data[:room]...)
	}
	c.markTruncated()
	return false
}

func (c *outputCollector) markTruncated() {
	if c.truncated {
		return
	}
	limit := transcriptOutputLimit - len(transcriptTruncationMarker)
	if len(c.buf) > limit {
		end := validUTF8Prefix(c.buf, limit)
		c.buf = c.buf[:end]
	}
	c.buf = append(c.buf, transcriptTruncationMarker...)
	c.truncated = true
}

func validUTF8Prefix(data []byte, limit int) int {
	if limit >= len(data) {
		return len(data)
	}
	if limit <= 0 {
		return 0
	}
	for limit > 0 && !utf8.RuneStart(data[limit]) {
		limit--
	}
	return limit
}

func bounded(value int, upper int) int {
	if value < 0 {
		return 0
	}
	if value >= upper {
		return upper - 1
	}
	return value
}

func boundedNonZero(value int, upper int) int {
	if value <= 0 {
		return 1
	}
	if value > upper {
		return upper
	}
	return value
}
