package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
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
const flashDuration = 1500 * time.Millisecond

var headerRe = regexp.MustCompile(`^([ \t│╷╵╶╴─├└┌┐┘╭╮╯╰~]*)?([@ox])\s{2}([0-9a-f]{10,40})(?:\s+.*)?$`)
var diffIDRe = regexp.MustCompile(`\bD[0-9]+\b`)
var ansiCSIRe = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)
var oscRe = regexp.MustCompile(`\x1b\].*?(\x07|\x1b\\)`)

var selectInput = syscall.Select

var progressFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

type commit struct {
	Hash          string
	FullHash      string
	Marker        string
	HeaderLine    int
	AnchorLine    int
	SubjectLine   int
	DiffID        string
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
	lines             []smartlogLine
	statusLines       []smartlogLine
	commits           []commit
	selected          int
	statusSelected    bool
	expanded          map[string]bool
	selectedHash      string
	lastRenderRows    int
	selectionStyle    lineStyle
	flashBackground   rgb
	flashBackgroundOK bool
	flashLines        map[int]bool
	flashStarted      time.Time
	markdownStyle     md.RenderStyle
	phabStatuses      map[string]string
	phabPending       map[string]bool
	phabResults       chan phabStatusResult
	progressFrame     int
	kaleidoscope      bool
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
	keyCtrlR
	keyEscape
	keyHelp
	keyGoto
	keyQuit
)

type keyBinding struct {
	keys        string
	description string
}

var keyBindings = []keyBinding{
	{keys: "Up / Down", description: "Move selection"},
	{keys: "Enter", description: "Go to selected commit"},
	{keys: "g", description: "Go to commit and keep SLR open"},
	{keys: "Space", description: "Toggle commit description"},
	{keys: "Ctrl-G", description: "Edit selected commit"},
	{keys: "Ctrl-D", description: "Open selected diff"},
	{keys: "Ctrl-R", description: "Refresh"},
	{keys: "?", description: "Toggle keybinding help"},
	{keys: "Esc", description: "Close help, or quit"},
	{keys: "q", description: "Quit"},
}

type rgb struct {
	r int
	g int
	b int
}

type lineStyle struct {
	start string
	end   string
}

type smartlogFetchResult struct {
	lines       []string
	statusLines []string
	err         error
}

type phabStatusResult struct {
	diffID string
	status string
	err    error
}

type repositoryObserver struct {
	changes <-chan struct{}
	close   func()
	retry   func()
}

type watchProjectResponse struct {
	Watch        string `json:"watch"`
	RelativePath string `json:"relative_path"`
}

func main() {
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		if err := printPlainView(); err != nil {
			exitWithError(err)
		}
		return
	}

	m, err := newModel()
	if err != nil {
		exitWithError(err)
	}
	if len(m.commits) == 0 {
		if err := printPlainView(); err != nil {
			exitWithError(err)
		}
		return
	}

	if err := runInteractive(m); err != nil {
		exitWithError(err)
	}
}

func newModel() (*model, error) {
	rawLines, rawStatusLines, err := fetchRepositoryViewWithProgress("loading")
	if err != nil {
		return nil, err
	}
	lines := makeSmartlogLines(rawLines)

	commits := parseCommits(lines)
	kaleidoscope := isFBSource()
	if kaleidoscope {
		resolveFullHashes(commits)
	}
	selected := 0
	for i, c := range commits {
		if c.Marker == "@" {
			selected = i
			break
		}
	}

	m := &model{
		lines:        lines,
		statusLines:  makeSmartlogLines(rawStatusLines),
		commits:      commits,
		selected:     selected,
		expanded:     map[string]bool{},
		selectedHash: commits[selected].Hash,
		phabStatuses: map[string]string{},
		phabPending:  map[string]bool{},
		phabResults:  make(chan phabStatusResult, 128),
		kaleidoscope: kaleidoscope,
	}
	startPhabStatusFetches(m)
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
	m.flashBackground, m.flashBackgroundOK = queryTerminalBackground()
	m.selectionStyle = selectionStyleForBackground(m.flashBackground, m.flashBackgroundOK)
	m.markdownStyle = detectMarkdownStyle()
	top := 0
	observer := startRepositoryObserver()
	defer observer.close()
	helpVisible := false

	for {
		if consumeRepositoryChange(observer.changes) {
			if err := refreshModel(m); err != nil {
				observer.retry()
			} else {
				top = 0
			}
		}
		processPhabStatusResults(m)
		rows, nextTop := 0, top
		if helpVisible {
			rows = renderHelpPopup(m.lastRenderRows)
		} else {
			rows, nextTop = render(m, top)
		}
		m.lastRenderRows = rows
		top = nextTop

		k, ok, err := readNextEvent(reader, fd, hasPendingPhabStatus(m) || observer.changes != nil)
		if err != nil {
			if errors.Is(err, io.EOF) {
				renderWithoutSelection(m, top)
				return nil
			}
			clearRenderArea(m.lastRenderRows)
			return err
		}
		if !ok {
			m.progressFrame++
			continue
		}
		if helpVisible {
			switch k {
			case keyHelp, keyEscape:
				helpVisible = false
			case keyQuit:
				renderWithoutSelection(m, top)
				return nil
			}
			continue
		}

		switch k {
		case keyUp:
			moveSelectionUp(m)
		case keyDown:
			moveSelectionDown(m)
		case keySpace:
			if m.statusSelected {
				break
			}
			if err := toggleExpanded(m); err != nil {
				return err
			}
		case keyCtrlG:
			if m.statusSelected {
				break
			}
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
			args := mdiffArgs(m)
			if err := suspendAndRun(m, origState, func() error {
				return runAttached("mdiff", args...)
			}); err != nil {
				return err
			}
			if err := refreshModel(m); err != nil {
				return err
			}
			top = 0
		case keyCtrlR:
			if err := refreshModel(m); err != nil {
				return err
			}
			top = 0
		case keyHelp:
			helpVisible = true
		case keyGoto:
			if m.statusSelected {
				break
			}
			if err := runQuiet("sl", "goto", currentCommit(m).Hash); err != nil {
				return err
			}
			if err := refreshModel(m); err != nil {
				return err
			}
			top = 0
		case keyEnter:
			if m.statusSelected {
				break
			}
			hash := currentCommit(m).Hash
			if err := runQuiet("sl", "goto", hash); err != nil {
				return err
			}
			if err := refreshModel(m); err != nil {
				return err
			}
			renderWithoutSelection(m, top)
			return nil
		case keyQuit, keyEscape:
			renderWithoutSelection(m, top)
			return nil
		}
	}
}

func moveSelectionUp(m *model) {
	if m.statusSelected {
		return
	}
	if m.selected > 0 {
		m.selected--
		m.selectedHash = m.commits[m.selected].Hash
		return
	}
	if len(m.statusLines) > 0 {
		m.statusSelected = true
	}
}

func moveSelectionDown(m *model) {
	if m.statusSelected {
		m.statusSelected = false
		m.selected = 0
		m.selectedHash = m.commits[0].Hash
		return
	}
	if m.selected < len(m.commits)-1 {
		m.selected++
		m.selectedHash = m.commits[m.selected].Hash
	}
}

func mdiffArgs(m *model) []string {
	if m.statusSelected {
		return []string{"-P"}
	}
	return []string{"-P", "-c", currentCommit(m).Hash}
}

func readNextEvent(reader *bufio.Reader, fd int, pending bool) (key, bool, error) {
	if !pending {
		k, err := readKey(reader, fd)
		return k, true, err
	}
	ok, err := waitForReaderInput(reader, fd, 80*time.Millisecond)
	if err != nil || !ok {
		return keyUnknown, false, err
	}
	k, err := readKey(reader, fd)
	return k, true, err
}

func startRepositoryObserver() *repositoryObserver {
	ctx, cancel := context.WithCancel(context.Background())
	rawChanges := make(chan struct{}, 1)
	changes := make(chan struct{}, 1)
	go func() {
		stop, err := startWatchmanSubscription(rawChanges)
		if err != nil {
			return
		}
		<-ctx.Done()
		stop()
	}()

	go monitorRepositoryChanges(ctx, rawChanges, changes, repositoryFingerprint, 200*time.Millisecond, 2*time.Second)
	return &repositoryObserver{
		changes: changes,
		close:   cancel,
		retry: func() {
			time.AfterFunc(time.Second, func() {
				select {
				case <-ctx.Done():
					return
				default:
					signalRepositoryChange(rawChanges)
				}
			})
		},
	}
}

func startWatchmanSubscription(changes chan<- struct{}) (func(), error) {
	rootOut, err := exec.Command("sl", "root").Output()
	if err != nil {
		return nil, err
	}
	root := strings.TrimSpace(string(rootOut))
	projectOut, err := exec.Command("watchman", "--no-pretty", "watch-project", root).Output()
	if err != nil {
		return nil, err
	}
	var project watchProjectResponse
	if err := json.Unmarshal(projectOut, &project); err != nil {
		return nil, err
	}
	if project.Watch == "" {
		return nil, errors.New("watchman returned an empty watch root")
	}

	cmd := exec.Command(
		"watchman",
		"--no-pretty",
		"-j",
		"-p",
		"--server-encoding=json",
		"--output-encoding=json",
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return nil, err
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		stdin.Close()
		return nil, err
	}

	query := map[string]any{
		"expression": []any{"anyof",
			[]any{"type", "f"},
			[]any{"type", "l"},
			[]any{"not", []any{"exists"}},
		},
		"fields":                  []string{"name"},
		"empty_on_fresh_instance": true,
	}
	if project.RelativePath != "" {
		query["relative_root"] = project.RelativePath
	}
	subscription := fmt.Sprintf("slr-%d", os.Getpid())
	if err := json.NewEncoder(stdin).Encode([]any{"subscribe", project.Watch, subscription, query}); err != nil {
		stdin.Close()
		cmd.Process.Kill()
		cmd.Wait()
		return nil, err
	}

	go func() {
		defer cmd.Wait()
		decoder := json.NewDecoder(stdout)
		for {
			var event map[string]any
			if err := decoder.Decode(&event); err != nil {
				return
			}
			if event["subscription"] != subscription {
				continue
			}
			files, _ := event["files"].([]any)
			if len(files) == 0 {
				continue
			}
			signalRepositoryChange(changes)
		}
	}()

	return func() {
		stdin.Close()
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
	}, nil
}

func monitorRepositoryChanges(
	ctx context.Context,
	rawChanges <-chan struct{},
	changes chan<- struct{},
	fingerprint func() (string, error),
	debounceDelay time.Duration,
	pollInterval time.Duration,
) {
	lastFingerprint, _ := fingerprint()
	pollTicker := time.NewTicker(pollInterval)
	defer pollTicker.Stop()

	var debounceTimer *time.Timer
	var debounce <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			if debounceTimer != nil {
				debounceTimer.Stop()
			}
			return
		case <-rawChanges:
			if debounceTimer == nil {
				debounceTimer = time.NewTimer(debounceDelay)
			} else {
				if !debounceTimer.Stop() {
					select {
					case <-debounceTimer.C:
					default:
					}
				}
				debounceTimer.Reset(debounceDelay)
			}
			debounce = debounceTimer.C
		case <-debounce:
			if current, err := fingerprint(); err == nil {
				lastFingerprint = current
			}
			signalRepositoryChange(changes)
			debounce = nil
		case <-pollTicker.C:
			current, err := fingerprint()
			if err != nil {
				continue
			}
			if lastFingerprint == "" {
				lastFingerprint = current
				continue
			}
			if current != lastFingerprint {
				lastFingerprint = current
				signalRepositoryChange(changes)
			}
		}
	}
}

func repositoryFingerprint() (string, error) {
	status, err := exec.Command("sl", "--pager=off", "status").Output()
	if err != nil {
		return "", err
	}
	log, err := exec.Command(
		"sl",
		"log",
		"-r",
		revset,
		"-T",
		"{node}\\0{desc}\\0{bookmarks}\\0{remotenames}\\n",
	).Output()
	if err != nil {
		return "", err
	}
	current, err := exec.Command("sl", "log", "-r", ".", "-T", "{node}\\n").Output()
	if err != nil {
		return "", err
	}
	digest := sha256.New()
	digest.Write(status)
	digest.Write([]byte{0})
	digest.Write(log)
	digest.Write([]byte{0})
	digest.Write(current)
	return fmt.Sprintf("%x", digest.Sum(nil)), nil
}

func signalRepositoryChange(changes chan<- struct{}) {
	select {
	case changes <- struct{}{}:
	default:
	}
}

func consumeRepositoryChange(changes <-chan struct{}) bool {
	changed := false
	for {
		select {
		case <-changes:
			changed = true
		default:
			return changed
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

func fetchStatus() ([]string, error) {
	command := "sl --pager=off status"
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

func fetchRepositoryView() ([]string, []string, error) {
	lines, err := fetchSmartlog()
	if err != nil {
		return nil, nil, err
	}
	statusLines, err := fetchStatus()
	if err != nil {
		return nil, nil, err
	}
	return lines, statusLines, nil
}

func fetchRepositoryViewWithProgress(label string) ([]string, []string, error) {
	return fetchRepositoryViewWithProgressMode(label, false)
}

func fetchRepositoryViewWithProgressMode(label string, preserveView bool) ([]string, []string, error) {
	if !term.IsTerminal(int(os.Stdout.Fd())) {
		return fetchRepositoryView()
	}

	results := make(chan smartlogFetchResult, 1)
	go func() {
		lines, statusLines, err := fetchRepositoryView()
		results <- smartlogFetchResult{lines: lines, statusLines: statusLines, err: err}
	}()

	ticker := time.NewTicker(80 * time.Millisecond)
	defer ticker.Stop()

	frame := 0
	if preserveView {
		fmt.Fprint(os.Stdout, "\r\n")
	}
	renderProgressFrame(label, frame)
	for {
		select {
		case result := <-results:
			clearProgressFrame()
			if preserveView {
				fmt.Fprint(os.Stdout, "\x1b[1A\r")
			}
			return result.lines, result.statusLines, result.err
		case <-ticker.C:
			frame++
			renderProgressFrame(label, frame)
		}
	}
}

func renderProgressFrame(label string, frame int) {
	fmt.Fprintf(os.Stdout, "\r\x1b[K%s %s", progressFrames[frame%len(progressFrames)], label)
}

func clearProgressFrame() {
	fmt.Fprint(os.Stdout, "\r\x1b[K")
}

func printPlainView() error {
	if err := runAttached("sl", "status"); err != nil {
		return err
	}
	return runAttached("sl", "sl", "-r", revset)
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
			DiffID:      extractDiffID(line.plain),
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

func extractDiffID(line string) string {
	return diffIDRe.FindString(line)
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
	m.flashLines = nil
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
	return renderWithSelection(m, top, true, false, true)
}

func renderWithoutSelection(m *model, top int) (int, int) {
	processPhabStatusResults(m)
	m.flashLines = nil
	return renderWithSelection(m, top, false, true, false)
}

func renderHelpPopup(previousRows int) int {
	termWidth, termHeight, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || termWidth <= 0 {
		termWidth = 80
	}
	if err != nil || termHeight <= 0 {
		termHeight = 24
	}

	lines := buildHelpPopup(termWidth)
	topPadding := max(0, (termHeight-1-len(lines))/2)
	clearRenderArea(previousRows)
	for i := 0; i < topPadding; i++ {
		fmt.Fprint(os.Stdout, "\r\n")
	}
	for i, line := range lines {
		leftPadding := max(0, (termWidth-displayWidth(line))/2)
		lineEnd := "\r\n"
		if i == len(lines)-1 {
			lineEnd = ""
		}
		fmt.Fprintf(os.Stdout, "\r%s%s%s", strings.Repeat(" ", leftPadding), line, lineEnd)
	}
	return topPadding + len(lines)
}

func buildHelpPopup(maxWidth int) []string {
	keyWidth := 0
	for _, binding := range keyBindings {
		keyWidth = max(keyWidth, displayWidth(binding.keys))
	}
	contentWidth := displayWidth("Keybindings")
	for _, binding := range keyBindings {
		contentWidth = max(contentWidth, keyWidth+2+displayWidth(binding.description))
	}
	if maxWidth > 0 {
		contentWidth = min(contentWidth, max(1, maxWidth-4))
	}

	border := "+" + strings.Repeat("-", contentWidth+2) + "+"
	lines := []string{
		border,
		"| " + padOrTrim("Keybindings", contentWidth) + " |",
		"| " + strings.Repeat(" ", contentWidth) + " |",
	}
	for _, binding := range keyBindings {
		text := fmt.Sprintf("%-*s  %s", keyWidth, binding.keys, binding.description)
		lines = append(lines, "| "+padOrTrim(text, contentWidth)+" |")
	}
	return append(lines, border)
}

func padOrTrim(text string, width int) string {
	runes := []rune(text)
	if len(runes) > width {
		return string(runes[:width])
	}
	return text + strings.Repeat(" ", width-len(runes))
}

func renderWithSelection(m *model, top int, highlightSelection bool, preserveViewport bool, showPendingPhab bool) (int, int) {
	rendered, selectedLine := buildRenderedLinesWithPending(m, showPendingPhab)
	flashStyle, flashActive := currentFlashStyle(m, time.Now())
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
	for i, line := range rendered {
		lineRows[i] = displayRows(line.plain, termWidth)
	}

	selectedEnd := selectedLine + 1
	if m.statusSelected {
		selectedEnd = selectedLine + len(m.statusLines)
	}
	top = adjustViewportTop(top, selectedLine, selectedEnd, lineRows, maxHeight, preserveViewport)

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
	statusStart := 0
	for i, line := range view {
		absoluteLine := top + i
		lineEnd := "\r\n"
		if i == len(view)-1 {
			lineEnd = ""
		}
		selected := renderedLineSelected(m, absoluteLine, selectedLine, statusStart)
		style := m.selectionStyle
		flashing := flashActive && m.flashLines[absoluteLine]
		if flashing {
			style = flashStyle
		}
		fmt.Fprintf(os.Stdout, "\r%s%s", formatRenderedLine(line.raw, (selected && highlightSelection) || flashing, style), lineEnd)
	}
	return usedRows, top
}

func renderedLineSelected(m *model, line, selectedLine, statusStart int) bool {
	if m.statusSelected {
		return line >= statusStart && line < statusStart+len(m.statusLines)
	}
	return line == selectedLine
}

func adjustViewportTop(top, selectedStart, selectedEnd int, lineRows []int, maxHeight int, preserveViewport bool) int {
	if top < 0 {
		top = 0
	}
	if len(lineRows) == 0 {
		return 0
	}

	totalRows := 0
	for _, rows := range lineRows {
		totalRows += rows
	}
	if totalRows <= maxHeight {
		return 0
	}
	if top >= len(lineRows) {
		top = len(lineRows) - 1
	}
	if preserveViewport {
		return top
	}
	if selectedStart < top {
		top = selectedStart
	}
	for top < selectedEnd-1 && visibleRowsBetween(lineRows, top, selectedEnd) > maxHeight {
		top++
	}
	return top
}

func buildRenderedLines(m *model) ([]smartlogLine, int) {
	return buildRenderedLinesWithPending(m, true)
}

func buildRenderedLinesWithPending(m *model, showPendingPhab bool) ([]smartlogLine, int) {
	headerByLine := make(map[int]int, len(m.commits))
	anchorByLine := make(map[int]int, len(m.commits))
	for i, c := range m.commits {
		headerByLine[c.HeaderLine] = i
		anchorByLine[c.AnchorLine] = i
	}

	rendered := make([]smartlogLine, 0, len(m.lines)+len(m.statusLines)+1)
	selectedLine := 0
	if len(m.statusLines) > 0 {
		if m.statusSelected {
			selectedLine = len(rendered)
		}
		rendered = append(rendered, m.statusLines...)
		rendered = append(rendered, smartlogLine{})
	}

	for i, line := range m.lines {
		if idx, ok := headerByLine[i]; ok && idx == m.selected && !m.statusSelected {
			selectedLine = len(rendered)
		}
		if idx, ok := headerByLine[i]; ok {
			if m.kaleidoscope {
				line = appendKaleidoscopeLink(line, m.commits[idx])
			}
			line = appendPhabStatus(line, m.commits[idx], m, showPendingPhab)
		}
		rendered = append(rendered, line)
		if idx, ok := anchorByLine[i]; ok && m.expanded[m.commits[idx].Hash] {
			rendered = append(rendered, m.commits[idx].BodyLines...)
		}
	}
	return rendered, selectedLine
}

func appendKaleidoscopeLink(line smartlogLine, c commit) smartlogLine {
	if c.FullHash == "" {
		return line
	}
	label := "ksdiff"
	return smartlogLine{
		raw:   line.raw + "  \x1b]8;;ksdiff://" + c.FullHash + "\x1b\\\x1b[1m" + label + "\x1b[22m\x1b]8;;\x1b\\",
		plain: line.plain + "  " + label,
	}
}

func appendPhabStatus(line smartlogLine, c commit, m *model, showPending bool) smartlogLine {
	if c.DiffID == "" {
		return line
	}
	status := m.phabStatuses[c.DiffID]
	if status == "" && showPending && m.phabPending[c.DiffID] {
		status = progressFrames[m.progressFrame%len(progressFrames)]
	}
	if status == "" {
		return line
	}
	annotation := "  " + status
	return smartlogLine{
		raw:   line.raw + annotation,
		plain: line.plain + annotation,
	}
}

func refreshModel(m *model) error {
	statusSelected := m.statusSelected
	before, _ := buildRenderedLinesWithPending(m, false)

	rawLines, rawStatusLines, err := fetchRepositoryViewWithProgressMode("refreshing", m.lastRenderRows > 0)
	if err != nil {
		return err
	}
	lines := makeSmartlogLines(rawLines)
	statusLines := makeSmartlogLines(rawStatusLines)
	commits := parseCommits(lines)
	if m.kaleidoscope {
		resolveFullHashes(commits)
	}
	if len(commits) == 0 {
		m.lines = lines
		m.statusLines = statusLines
		m.commits = nil
		m.expanded = map[string]bool{}
		m.selected = 0
		m.selectedHash = ""
		m.statusSelected = false
		startChangedLineFlash(m, before)
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
	m.statusLines = statusLines
	m.commits = commits
	m.selected = selected
	m.selectedHash = commits[selected].Hash
	m.expanded = newExpanded
	m.statusSelected = statusSelected && len(statusLines) > 0
	startPhabStatusFetches(m)
	startChangedLineFlash(m, before)
	return nil
}

func startChangedLineFlash(m *model, before []smartlogLine) {
	after, _ := buildRenderedLinesWithPending(m, false)
	m.flashLines = changedRenderedLineIndices(before, after)
	if len(m.flashLines) > 0 {
		m.flashStarted = time.Now()
	}
}

func changedRenderedLineIndices(before, after []smartlogLine) map[int]bool {
	n := len(before)
	m := len(after)
	if n > 0 && m > 1_000_000/n {
		return changedRenderedLineRange(before, after)
	}
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if before[i].plain == after[j].plain {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}

	changed := map[int]bool{}
	i, j := 0, 0
	for i < n && j < m {
		if before[i].plain == after[j].plain {
			i++
			j++
			continue
		}
		if lcs[i+1][j] >= lcs[i][j+1] {
			changed[j] = true
			i++
		} else {
			changed[j] = true
			j++
		}
	}
	for ; j < m; j++ {
		changed[j] = true
	}
	if i < n && m > 0 {
		changed[m-1] = true
	}
	return changed
}

func changedRenderedLineRange(before, after []smartlogLine) map[int]bool {
	prefix := 0
	for prefix < len(before) && prefix < len(after) && before[prefix].plain == after[prefix].plain {
		prefix++
	}
	suffix := 0
	for suffix < len(before)-prefix && suffix < len(after)-prefix &&
		before[len(before)-1-suffix].plain == after[len(after)-1-suffix].plain {
		suffix++
	}

	changed := map[int]bool{}
	for i := prefix; i < len(after)-suffix; i++ {
		changed[i] = true
	}
	if len(changed) == 0 && prefix < len(before) && len(after) > 0 {
		changed[min(prefix, len(after)-1)] = true
	}
	return changed
}

func isFBSource() bool {
	cmd := exec.Command("sl", "config", "remotefilelog.reponame")
	out, err := cmd.Output()
	return err == nil && strings.TrimSpace(string(out)) == "fbsource"
}

func resolveFullHashes(commits []commit) {
	cmd := exec.Command("sl", "log", "-r", revset, "-T", "{node}\\n")
	out, err := cmd.Output()
	if err != nil {
		return
	}

	assignFullHashes(commits, strings.Fields(string(out)))
}

func assignFullHashes(commits []commit, fullHashes []string) {
	for i := range commits {
		for _, fullHash := range fullHashes {
			if strings.HasPrefix(fullHash, commits[i].Hash) {
				commits[i].FullHash = fullHash
				break
			}
		}
	}
}

func startPhabStatusFetches(m *model) {
	if m.phabStatuses == nil {
		m.phabStatuses = map[string]string{}
	}
	if m.phabPending == nil {
		m.phabPending = map[string]bool{}
	}
	if m.phabResults == nil {
		m.phabResults = make(chan phabStatusResult, 128)
	}

	seen := map[string]bool{}
	for _, c := range m.commits {
		if c.DiffID == "" || seen[c.DiffID] {
			continue
		}
		seen[c.DiffID] = true
		if m.phabStatuses[c.DiffID] != "" || m.phabPending[c.DiffID] {
			continue
		}
		m.phabPending[c.DiffID] = true
		go func(diffID string) {
			status, err := fetchPhabStatus(diffID)
			m.phabResults <- phabStatusResult{diffID: diffID, status: status, err: err}
		}(c.DiffID)
	}
}

func processPhabStatusResults(m *model) bool {
	changed := false
	for {
		select {
		case result := <-m.phabResults:
			delete(m.phabPending, result.diffID)
			if result.err == nil && result.status != "" {
				m.phabStatuses[result.diffID] = result.status
			}
			changed = true
		default:
			return changed
		}
	}
}

func hasPendingPhabStatus(m *model) bool {
	for _, c := range m.commits {
		if c.DiffID != "" && m.phabPending[c.DiffID] {
			return true
		}
	}
	return false
}

func fetchPhabStatus(diffID string) (string, error) {
	status, err := fetchMetaPhabStatus(diffID)
	if err == nil && status != "" {
		return status, nil
	}
	fallback, fallbackErr := fetchJellyfishPhabStatus(diffID)
	if fallbackErr == nil && fallback != "" {
		return fallback, nil
	}
	if err != nil {
		return "", err
	}
	return "", fallbackErr
}

func fetchMetaPhabStatus(diffID string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "meta", "phabricator.diff", "describe", "--number="+diffID, "--output=json", "--no-color")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}

	var data map[string]any
	if err := json.Unmarshal(out, &data); err != nil {
		return "", err
	}
	return phabStatusFromMap(data), nil
}

func fetchJellyfishPhabStatus(diffID string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "jf", "--json", "diff-properties", diffID)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}

	var wrapper map[string]any
	if err := json.Unmarshal(out, &wrapper); err != nil {
		return "", err
	}
	data, _ := wrapper["data"].(map[string]any)
	properties, _ := data["diff-properties"].(map[string]any)
	return phabStatusFromMap(properties), nil
}

func phabStatusFromMap(data map[string]any) string {
	if len(data) == 0 {
		return ""
	}
	if jsonBool(data["is_landed"]) {
		return "landed"
	}
	if jsonBool(data["is_landing"]) {
		return "landing"
	}
	status, _ := data["status"].(string)
	return humanStatus(status)
}

func jsonBool(value any) bool {
	switch v := value.(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(v, "true")
	default:
		return false
	}
}

func humanStatus(status string) string {
	status = strings.TrimSpace(status)
	if status == "" {
		return ""
	}
	status = strings.ReplaceAll(status, "_", " ")
	return strings.ToLower(status)
}

func suspendAndRun(m *model, origState *term.State, fn func() error) error {
	fd := int(os.Stdin.Fd())
	clearRenderArea(m.lastRenderRows)
	m.lastRenderRows = 0
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
	case '?':
		return keyHelp, nil
	case 'g':
		return keyGoto, nil
	case '\r', '\n':
		return keyEnter, nil
	case ' ':
		return keySpace, nil
	case 0x04:
		return keyCtrlD, nil
	case 0x07:
		return keyCtrlG, nil
	case 0x12:
		return keyCtrlR, nil
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
	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false, nil
		}

		var readfds syscall.FdSet
		fdSet(fd, &readfds)
		tv := syscall.NsecToTimeval(remaining.Nanoseconds())
		n, err := selectInput(fd+1, &readfds, nil, nil, &tv)
		if err != nil {
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			return false, err
		}
		return n > 0, nil
	}
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

func runQuiet(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdin = os.Stdin
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stripControlForError(stderr.String()))
		if msg == "" {
			msg = strings.TrimSpace(stripControlForError(stdout.String()))
		}
		if msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
		return err
	}
	return nil
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

func formatRenderedLine(raw string, selected bool, style lineStyle) string {
	if !selected {
		return raw
	}
	if style.start != "" {
		return style.start + decorateSelected(raw, style.start) + style.end
	}
	return "\x1b[1m" + raw + "\x1b[0m"
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

func selectionStyleForBackground(bg rgb, ok bool) lineStyle {
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

func currentFlashStyle(m *model, now time.Time) (lineStyle, bool) {
	if len(m.flashLines) == 0 {
		return lineStyle{}, false
	}
	elapsed := now.Sub(m.flashStarted)
	if elapsed < 0 || elapsed >= flashDuration {
		m.flashLines = nil
		return lineStyle{}, false
	}

	progress := float64(elapsed) / float64(flashDuration)
	pulse := 0.5 + 0.5*math.Sin(2*math.Pi*elapsed.Seconds()/0.45)
	intensity := pulse * (1 - progress)
	if !m.flashBackgroundOK {
		if intensity < 0.25 {
			return lineStyle{}, false
		}
		return lineStyle{start: "\x1b[7m\x1b[1m", end: "\x1b[0m"}, true
	}

	overlay := rgb{255, 190, 60}
	if luminance(m.flashBackground) > 128.0 {
		overlay = rgb{180, 95, 0}
	}
	tint := blend(m.flashBackground, overlay, 0.32*intensity)
	return lineStyle{
		start: fmt.Sprintf("\x1b[48;2;%d;%d;%dm\x1b[1m", tint.r, tint.g, tint.b),
		end:   "\x1b[0m",
	}, true
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
