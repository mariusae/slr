package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mariusae/md"
	"golang.org/x/term"
)

const revset = "draft() & ((::.) + (.::))"

var headerRe = regexp.MustCompile(`^([ \t│╷╵╶╴─├└┌┐┘╭╮╯╰~]*)?([@ox])\s{2}([0-9a-f]{10,40})(?:\s+.*)?$`)
var ansiCSIRe = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)
var oscRe = regexp.MustCompile(`\x1b\].*?(\x07|\x1b\\)`)

type commit struct {
	Hash          string
	Marker        string
	HeaderLine    int
	AnchorLine    int
	SubjectLine   int
	SubjectText   string
	BodyPrefix    string
	ExpandPrefix  string
	BodyLines     []smartlogLine
	Description   string
	DescriptionOK bool
}

type smartlogLine struct {
	raw   string
	plain string
}

type model struct {
	lines          []smartlogLine
	commits        []commit
	selected       int
	expanded       map[string]bool
	selectedHash   string
	lastRenderRows int
	selectionStyle lineStyle
	markdownStyle  md.RenderStyle
}

type key int

const (
	keyUnknown key = iota
	keyUp
	keyDown
	keyEnter
	keySpace
	keyCtrlG
	keyCtrlD
	keyEscape
	keyQuit
)

type rgb struct {
	r int
	g int
	b int
}

type lineStyle struct {
	start string
	end   string
}

func main() {
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		if err := runAttached("sl", "sl", "-r", revset); err != nil {
			exitWithError(err)
		}
		return
	}

	m, err := newModel()
	if err != nil {
		exitWithError(err)
	}
	if len(m.commits) == 0 {
		if err := printPlainSmartlog(); err != nil {
			exitWithError(err)
		}
		return
	}

	if err := runInteractive(m); err != nil {
		exitWithError(err)
	}
}

func newModel() (*model, error) {
	rawLines, err := fetchSmartlog()
	if err != nil {
		return nil, err
	}
	lines := makeSmartlogLines(rawLines)

	commits := parseCommits(lines)
	selected := 0
	for i, c := range commits {
		if c.Marker == "@" {
			selected = i
			break
		}
	}

	m := &model{
		lines:        lines,
		commits:      commits,
		selected:     selected,
		expanded:     map[string]bool{},
		selectedHash: commits[selected].Hash,
	}
	return m, nil
}

func runInteractive(m *model) error {
	fd := int(os.Stdin.Fd())
	origState, err := term.MakeRaw(fd)
	if err != nil {
		return err
	}
	defer term.Restore(fd, origState)
	hideCursor()
	defer showCursor()

	reader := bufio.NewReader(os.Stdin)
	m.selectionStyle = detectSelectionStyle()
	m.markdownStyle = detectMarkdownStyle()
	top := 0

	for {
		rows, nextTop := render(m, top)
		m.lastRenderRows = rows
		top = nextTop

		k, err := readKey(reader, fd)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			clearRenderArea(m.lastRenderRows)
			return err
		}

		switch k {
		case keyUp:
			if m.selected > 0 {
				m.selected--
				m.selectedHash = m.commits[m.selected].Hash
			}
		case keyDown:
			if m.selected < len(m.commits)-1 {
				m.selected++
				m.selectedHash = m.commits[m.selected].Hash
			}
		case keySpace:
			if err := toggleExpanded(m); err != nil {
				return err
			}
		case keyCtrlG:
			hash := currentCommit(m).Hash
			if err := suspendAndRun(m, origState, func() error {
				return runAttached("sl", "metaedit", "-r", hash)
			}); err != nil {
				return err
			}
			if err := refreshModel(m); err != nil {
				return err
			}
			top = 0
		case keyCtrlD:
			hash := currentCommit(m).Hash
			if err := suspendAndRun(m, origState, func() error {
				return runAttached("mdiff", "-c", hash)
			}); err != nil {
				return err
			}
			if err := refreshModel(m); err != nil {
				return err
			}
			top = 0
		case keyEnter:
			hash := currentCommit(m).Hash
			showCursor()
			term.Restore(fd, origState)
			return runAttached("sl", "goto", hash)
		case keyQuit, keyEscape:
			return nil
		}
	}
}

func fetchSmartlog() ([]string, error) {
	command := fmt.Sprintf("sl --pager=off sl -r %s", shellSingleQuote(revset))
	cmd := exec.Command("script", "-qefc", command, "/dev/null")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stdout
	if err := cmd.Run(); err != nil {
		if stdout.Len() > 0 {
			return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(stripControlForError(stdout.String())))
		}
		return nil, err
	}
	text := normalizeSmartlogOutput(stdout.String())
	if text == "" {
		return []string{}, nil
	}
	return strings.Split(text, "\n"), nil
}

func printPlainSmartlog() error {
	cmd := exec.Command("sl", "sl", "-r", revset)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func parseCommits(lines []smartlogLine) []commit {
	commits := make([]commit, 0)
	for i, line := range lines {
		match := headerRe.FindStringSubmatch(line.plain)
		if match == nil {
			continue
		}
		commits = append(commits, commit{
			Hash:        match[3],
			Marker:      match[2],
			HeaderLine:  i,
			AnchorLine:  i,
			SubjectLine: -1,
		})
	}

	for i := range commits {
		end := len(lines)
		if i+1 < len(commits) {
			end = commits[i+1].HeaderLine
		}
		for lineIndex := commits[i].HeaderLine + 1; lineIndex < end; lineIndex++ {
			prefix, content, ok := splitContentLine(lines[lineIndex].plain)
			if !ok {
				continue
			}
			commits[i].SubjectLine = lineIndex
			commits[i].AnchorLine = lineIndex
			commits[i].SubjectText = content
			commits[i].BodyPrefix = prefix
			commits[i].ExpandPrefix = deriveExpandPrefix(prefix, lines[lineIndex+1:end])
			break
		}
	}

	return commits
}

func splitContentLine(line string) (string, string, bool) {
	for i, r := range line {
		if r == ' ' || r == '\t' {
			continue
		}
		if i < 2 || line[i-2:i] != "  " {
			continue
		}
		prefix := line[:i]
		if !containsGraphRune(prefix) {
			continue
		}
		return prefix, line[i:], true
	}
	return "", "", false
}

func containsGraphRune(s string) bool {
	for _, r := range s {
		if isGraphRune(r) {
			return true
		}
	}
	return false
}

func isGraphRune(r rune) bool {
	switch r {
	case '│', '╷', '╵', '╶', '╴', '─', '├', '└', '┌', '┐', '┘', '╭', '╮', '╯', '╰', '~':
		return true
	default:
		return false
	}
}

func deriveExpandPrefix(subjectPrefix string, trailing []smartlogLine) string {
	targetWidth := displayWidth(subjectPrefix)
	for _, line := range trailing {
		if line.plain == "" {
			continue
		}
		if _, _, ok := splitContentLine(line.plain); ok {
			continue
		}
		return padPrefixWidth(line.plain, targetWidth)
	}
	return padPrefixWidth(normalizeGraphPrefix(subjectPrefix), targetWidth)
}

func padPrefixWidth(prefix string, targetWidth int) string {
	width := displayWidth(prefix)
	if width >= targetWidth {
		return prefix
	}
	return prefix + strings.Repeat(" ", targetWidth-width)
}

func normalizeGraphPrefix(prefix string) string {
	graph := strings.TrimRight(prefix, " ")
	if graph == "" {
		return prefix
	}

	var out []rune
	for _, r := range graph {
		out = append(out, normalizeGraphRune(r))
	}
	return string(out)
}

func normalizeGraphRune(r rune) rune {
	switch {
	case isGraphRune(r):
		return '│'
	case r == ' ', r == '\t':
		return r
	default:
		return ' '
	}
}

func currentCommit(m *model) *commit {
	return &m.commits[m.selected]
}

func toggleExpanded(m *model) error {
	c := currentCommit(m)
	if m.expanded[c.Hash] {
		delete(m.expanded, c.Hash)
		return nil
	}
	if !c.DescriptionOK {
		desc, err := fetchDescription(c.Hash)
		if err != nil {
			return err
		}
		c.Description = desc
		c.DescriptionOK = true
	}
	c.BodyLines = renderExpansionBody(*c, expansionRenderWidth(*c), m.markdownStyle)
	m.commits[m.selected] = *c
	if len(c.BodyLines) == 0 {
		return nil
	}
	m.expanded[c.Hash] = true
	return nil
}

func fetchDescription(hash string) (string, error) {
	cmd := exec.Command("sl", "log", "-r", hash, "-T", "{desc}\n")
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if stderr.Len() > 0 {
			return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
		}
		return "", err
	}
	return strings.TrimRight(strings.ReplaceAll(stdout.String(), "\r\n", "\n"), "\n"), nil
}

func renderExpansionBody(c commit, width int, style md.RenderStyle) []smartlogLine {
	if c.Description == "" || c.BodyPrefix == "" {
		return nil
	}
	prefix := c.ExpandPrefix
	if prefix == "" {
		prefix = deriveExpandPrefix(c.BodyPrefix, nil)
	}

	source := c.Description
	if c.SubjectText != "" {
		prefix := c.SubjectText + "\n"
		if strings.HasPrefix(source, prefix) {
			source = strings.TrimPrefix(source, prefix)
		} else if source == c.SubjectText {
			source = ""
		}
	}
	source = strings.TrimLeft(source, "\n")
	source = strings.TrimRight(source, "\n")
	if source == "" {
		return nil
	}
	if width < 20 {
		width = 20
	}

	var buf bytes.Buffer
	if err := md.RenderWithStyle([]byte(source), &buf, width, true, style); err != nil {
		return prependBlankLine(prefixPlainBody(prefix, source), prefix)
	}
	rendered := normalizeSmartlogOutput(buf.String())
	if rendered == "" {
		return nil
	}
	lines := strings.Split(rendered, "\n")
	body := make([]smartlogLine, 0, len(lines))
	blankPrefix := strings.TrimRight(prefix, " ")
	body = append(body, smartlogLine{raw: blankPrefix, plain: blankPrefix})
	for _, line := range lines {
		raw := blankPrefix
		plain := blankPrefix
		if line != "" {
			raw = prefix + line
			plain = prefix + stripControl(line)
		}
		body = append(body, smartlogLine{raw: raw, plain: plain})
	}
	return trimTrailingBlankLines(body, blankPrefix)
}

func render(m *model, top int) (int, int) {
	rendered, selectedLine := buildRenderedLines(m)
	termWidth, termHeight, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || termHeight <= 0 {
		termHeight = 24
	}
	if err != nil || termWidth <= 0 {
		termWidth = 80
	}
	maxHeight := termHeight - 1
	if maxHeight < 5 {
		maxHeight = termHeight
	}
	if maxHeight < 1 {
		maxHeight = 1
	}

	lineRows := make([]int, len(rendered))
	totalRows := 0
	for i, line := range rendered {
		lineRows[i] = displayRows(line.plain, termWidth)
		totalRows += lineRows[i]
	}

	if selectedLine < top {
		top = selectedLine
	}
	for visibleRowsBetween(lineRows, top, selectedLine+1) > maxHeight {
		top++
	}
	if top < 0 {
		top = 0
	}
	if totalRows <= maxHeight {
		top = 0
	}

	end := top
	usedRows := 0
	for end < len(rendered) {
		nextRows := lineRows[end]
		if usedRows > 0 && usedRows+nextRows > maxHeight {
			break
		}
		usedRows += nextRows
		end++
	}
	view := rendered[top:end]

	clearRenderArea(m.lastRenderRows)
	for i, line := range view {
		absoluteLine := top + i
		lineEnd := "\r\n"
		if i == len(view)-1 {
			lineEnd = ""
		}
		if absoluteLine == selectedLine {
			if m.selectionStyle.start != "" {
				fmt.Fprintf(os.Stdout, "\r%s%s%s%s", m.selectionStyle.start, decorateSelected(line.raw, m.selectionStyle.start), m.selectionStyle.end, lineEnd)
			} else {
				fmt.Fprintf(os.Stdout, "\r\x1b[1m%s\x1b[0m%s", line.raw, lineEnd)
			}
			continue
		}
		fmt.Fprintf(os.Stdout, "\r%s%s", line.raw, lineEnd)
	}
	return usedRows, top
}

func buildRenderedLines(m *model) ([]smartlogLine, int) {
	headerByLine := make(map[int]int, len(m.commits))
	anchorByLine := make(map[int]int, len(m.commits))
	for i, c := range m.commits {
		headerByLine[c.HeaderLine] = i
		anchorByLine[c.AnchorLine] = i
	}

	rendered := make([]smartlogLine, 0, len(m.lines))
	selectedLine := 0

	for i, line := range m.lines {
		if idx, ok := headerByLine[i]; ok && idx == m.selected {
			selectedLine = len(rendered)
		}
		rendered = append(rendered, line)
		if idx, ok := anchorByLine[i]; ok && m.expanded[m.commits[idx].Hash] {
			rendered = append(rendered, m.commits[idx].BodyLines...)
		}
	}

	return rendered, selectedLine
}

func refreshModel(m *model) error {
	rawLines, err := fetchSmartlog()
	if err != nil {
		return err
	}
	lines := makeSmartlogLines(rawLines)
	commits := parseCommits(lines)
	if len(commits) == 0 {
		m.lines = lines
		m.commits = nil
		m.expanded = map[string]bool{}
		m.selected = 0
		m.selectedHash = ""
		return nil
	}

	selected := 0
	for i, c := range commits {
		if c.Hash == m.selectedHash {
			selected = i
			break
		}
	}

	newExpanded := map[string]bool{}
	oldByHash := make(map[string]commit, len(m.commits))
	for _, c := range m.commits {
		oldByHash[c.Hash] = c
	}
	for i := range commits {
		if old, ok := oldByHash[commits[i].Hash]; ok {
			commits[i].Description = old.Description
			commits[i].DescriptionOK = old.DescriptionOK
			if commits[i].DescriptionOK {
				commits[i].BodyLines = renderExpansionBody(commits[i], expansionRenderWidth(commits[i]), m.markdownStyle)
			}
			if m.expanded[commits[i].Hash] && len(commits[i].BodyLines) > 0 {
				newExpanded[commits[i].Hash] = true
			}
		}
	}

	m.lines = lines
	m.commits = commits
	m.selected = selected
	m.selectedHash = commits[selected].Hash
	m.expanded = newExpanded
	return nil
}

func suspendAndRun(m *model, origState *term.State, fn func() error) error {
	fd := int(os.Stdin.Fd())
	clearRenderArea(m.lastRenderRows)
	showCursor()
	if err := term.Restore(fd, origState); err != nil {
		return err
	}
	runErr := fn()
	_, err := term.MakeRaw(fd)
	if err != nil {
		return err
	}
	hideCursor()
	return runErr
}

func clearRenderArea(height int) {
	if height <= 0 {
		return
	}
	if height > 1 {
		fmt.Fprintf(os.Stdout, "\x1b[%dA", height-1)
	}
	fmt.Fprint(os.Stdout, "\r\x1b[J")
}

func hideCursor() {
	fmt.Fprint(os.Stdout, "\x1b[?25l")
}

func showCursor() {
	fmt.Fprint(os.Stdout, "\x1b[?25h")
}

func readKey(reader *bufio.Reader, fd int) (key, error) {
	b, err := reader.ReadByte()
	if err != nil {
		return keyUnknown, err
	}
	switch b {
	case 'q':
		return keyQuit, nil
	case '\r', '\n':
		return keyEnter, nil
	case ' ':
		return keySpace, nil
	case 0x04:
		return keyCtrlD, nil
	case 0x07:
		return keyCtrlG, nil
	case 0x1b:
		return readEscape(reader, fd)
	default:
		return keyUnknown, nil
	}
}

func readEscape(reader *bufio.Reader, fd int) (key, error) {
	ok, err := waitForReaderInput(reader, fd, 35*time.Millisecond)
	if err != nil || !ok {
		return keyEscape, err
	}
	next, err := reader.ReadByte()
	if err != nil {
		return keyEscape, err
	}

	switch next {
	case '[':
		return readCSISequence(reader, fd)
	case 'O':
		return readSS3Sequence(reader, fd)
	default:
		return keyUnknown, nil
	}
}

func readCSISequence(reader *bufio.Reader, fd int) (key, error) {
	ok, err := waitForReaderInput(reader, fd, 35*time.Millisecond)
	if err != nil || !ok {
		return keyUnknown, err
	}
	var seq []byte
	for {
		final, err := reader.ReadByte()
		if err != nil {
			return keyUnknown, err
		}
		seq = append(seq, final)
		if final >= 0x40 && final <= 0x7e {
			return decodeCursorFinal(final), nil
		}
		ok, err = waitForReaderInput(reader, fd, 35*time.Millisecond)
		if err != nil || !ok {
			return keyUnknown, err
		}
	}
}

func readSS3Sequence(reader *bufio.Reader, fd int) (key, error) {
	ok, err := waitForReaderInput(reader, fd, 35*time.Millisecond)
	if err != nil || !ok {
		return keyUnknown, err
	}
	final, err := reader.ReadByte()
	if err != nil {
		return keyUnknown, err
	}
	return decodeCursorFinal(final), nil
}

func decodeCursorFinal(final byte) key {
	switch final {
	case 'A':
		return keyUp
	case 'B':
		return keyDown
	default:
		return keyUnknown
	}
}

func waitForInput(fd int, timeout time.Duration) (bool, error) {
	var readfds syscall.FdSet
	fdSet(fd, &readfds)
	tv := syscall.NsecToTimeval(timeout.Nanoseconds())
	n, err := syscall.Select(fd+1, &readfds, nil, nil, &tv)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func waitForReaderInput(reader *bufio.Reader, fd int, timeout time.Duration) (bool, error) {
	if reader.Buffered() > 0 {
		return true, nil
	}
	return waitForInput(fd, timeout)
}

func fdSet(fd int, set *syscall.FdSet) {
	set.Bits[fd/64] |= 1 << (uint(fd) % 64)
}

func runAttached(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func exitWithError(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

func makeSmartlogLines(rawLines []string) []smartlogLine {
	lines := make([]smartlogLine, 0, len(rawLines))
	for _, raw := range rawLines {
		lines = append(lines, smartlogLine{
			raw:   raw,
			plain: stripControl(raw),
		})
	}
	return lines
}

func normalizeSmartlogOutput(out string) string {
	out = strings.ReplaceAll(out, "\r\n", "\n")
	out = strings.ReplaceAll(out, "\r", "\n")
	out = strings.TrimRight(out, "\n")
	return out
}

func stripControl(s string) string {
	s = oscRe.ReplaceAllString(s, "")
	s = ansiCSIRe.ReplaceAllString(s, "")
	return s
}

func stripControlForError(s string) string {
	return strings.TrimSpace(stripControl(normalizeSmartlogOutput(s)))
}

func decorateSelected(raw string, styleStart string) string {
	raw = strings.ReplaceAll(raw, "\x1b[0m", "\x1b[0m"+styleStart)
	raw = strings.ReplaceAll(raw, "\x1b[m", "\x1b[m"+styleStart)
	return raw
}

func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

func prefixPlainBody(prefix, source string) []smartlogLine {
	lines := strings.Split(source, "\n")
	body := make([]smartlogLine, 0, len(lines))
	blankPrefix := strings.TrimRight(prefix, " ")
	for _, line := range lines {
		raw := blankPrefix
		plain := blankPrefix
		if line != "" {
			raw = prefix + line
			plain = prefix + line
		}
		body = append(body, smartlogLine{raw: raw, plain: plain})
	}
	return trimTrailingBlankLines(body, blankPrefix)
}

func prependBlankLine(lines []smartlogLine, prefix string) []smartlogLine {
	blankPrefix := strings.TrimRight(prefix, " ")
	body := make([]smartlogLine, 0, len(lines)+1)
	body = append(body, smartlogLine{raw: blankPrefix, plain: blankPrefix})
	body = append(body, lines...)
	return trimTrailingBlankLines(body, blankPrefix)
}

func trimTrailingBlankLines(lines []smartlogLine, blankPrefix string) []smartlogLine {
	for len(lines) > 0 && lines[len(lines)-1].plain == blankPrefix {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func terminalWidth() int {
	width, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || width <= 0 {
		return 80
	}
	return width
}

func displayWidth(s string) int {
	return len([]rune(stripControl(s)))
}

func displayRows(s string, width int) int {
	if width <= 0 {
		return 1
	}
	lineWidth := displayWidth(s)
	if lineWidth == 0 {
		return 1
	}
	return (lineWidth-1)/width + 1
}

func visibleRowsBetween(lineRows []int, start, end int) int {
	if start < 0 {
		start = 0
	}
	if end > len(lineRows) {
		end = len(lineRows)
	}
	total := 0
	for i := start; i < end; i++ {
		total += lineRows[i]
	}
	return total
}

func expansionRenderWidth(c commit) int {
	prefix := c.ExpandPrefix
	if prefix == "" {
		prefix = c.BodyPrefix
	}
	width := terminalWidth() - displayWidth(prefix)
	if width > 100 {
		width = 100
	}
	if width < 20 {
		return 20
	}
	return width
}

func detectSelectionStyle() lineStyle {
	bg, ok := queryTerminalBackground()
	if !ok {
		return lineStyle{}
	}

	light := luminance(bg) > 128.0
	alpha := 0.20
	overlay := rgb{255, 255, 255}
	if light {
		alpha = 0.10
		overlay = rgb{0, 0, 0}
	}
	tint := blend(bg, overlay, alpha)
	return lineStyle{
		start: fmt.Sprintf("\x1b[48;2;%d;%d;%dm\x1b[1m", tint.r, tint.g, tint.b),
		end:   "\x1b[0m",
	}
}

func detectMarkdownStyle() md.RenderStyle {
	style, err := md.DetectRenderStyle()
	if err != nil {
		return md.RenderStyle{}
	}
	return style
}

func queryTerminalBackground() (rgb, bool) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return rgb{}, false
	}
	defer tty.Close()

	if _, err := tty.WriteString("\x1b]11;?\x1b\\"); err != nil {
		return rgb{}, false
	}

	reply, err := readOSCReply(tty, 2*time.Second)
	if err != nil {
		return rgb{}, false
	}

	color, ok := parseOSCColorResponse(reply)
	return color, ok
}

func readOSCReply(file *os.File, timeout time.Duration) ([]byte, error) {
	deadline := time.Now().Add(timeout)
	buf := make([]byte, 0, 64)
	tmp := make([]byte, 1)
	fd := int(file.Fd())

	for time.Now().Before(deadline) {
		remaining := time.Until(deadline)
		ok, err := waitForInput(fd, remaining)
		if err != nil {
			return nil, err
		}
		if !ok {
			break
		}
		n, err := file.Read(tmp)
		if err != nil {
			return nil, err
		}
		if n == 0 {
			continue
		}
		buf = append(buf, tmp[0])
		if len(buf) >= 2 && buf[len(buf)-1] == '\a' {
			return buf, nil
		}
		if len(buf) >= 2 && buf[len(buf)-2] == 0x1b && buf[len(buf)-1] == '\\' {
			return buf, nil
		}
	}

	return nil, errors.New("timed out waiting for terminal background reply")
}

func parseOSCColorResponse(reply []byte) (rgb, bool) {
	const prefix = "\x1b]11;"
	if !bytes.HasPrefix(reply, []byte(prefix)) {
		return rgb{}, false
	}

	payload := reply[len(prefix):]
	if len(payload) == 0 {
		return rgb{}, false
	}
	switch {
	case payload[len(payload)-1] == '\a':
		payload = payload[:len(payload)-1]
	case len(payload) >= 2 && payload[len(payload)-2] == 0x1b && payload[len(payload)-1] == '\\':
		payload = payload[:len(payload)-2]
	default:
		return rgb{}, false
	}

	text := string(payload)
	switch {
	case strings.HasPrefix(text, "rgb:"):
		return parseRGBSpec(strings.TrimPrefix(text, "rgb:"))
	case strings.HasPrefix(text, "rgba:"):
		return parseRGBSpec(strings.TrimPrefix(text, "rgba:"))
	default:
		return rgb{}, false
	}
}

func parseRGBSpec(spec string) (rgb, bool) {
	parts := strings.Split(spec, "/")
	if len(parts) < 3 {
		return rgb{}, false
	}
	r, ok := parseHexComponent(parts[0])
	if !ok {
		return rgb{}, false
	}
	g, ok := parseHexComponent(parts[1])
	if !ok {
		return rgb{}, false
	}
	b, ok := parseHexComponent(parts[2])
	if !ok {
		return rgb{}, false
	}
	return rgb{r: r, g: g, b: b}, true
}

func parseHexComponent(part string) (int, bool) {
	if len(part) != 2 && len(part) != 4 {
		return 0, false
	}
	value, err := strconv.ParseUint(part, 16, 32)
	if err != nil {
		return 0, false
	}
	if len(part) == 2 {
		return int(value), true
	}
	return int(value / 257), true
}

func luminance(c rgb) float64 {
	return 0.299*float64(c.r) + 0.587*float64(c.g) + 0.114*float64(c.b)
}

func blend(bg, overlay rgb, alpha float64) rgb {
	return rgb{
		r: blendChannel(bg.r, overlay.r, alpha),
		g: blendChannel(bg.g, overlay.g, alpha),
		b: blendChannel(bg.b, overlay.b, alpha),
	}
}

func blendChannel(bg, overlay int, alpha float64) int {
	return int(math.Floor(float64(overlay)*alpha + float64(bg)*(1.0-alpha)))
}
