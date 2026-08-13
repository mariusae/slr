package main

import (
	"bufio"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mariusae/md"
)

func TestParseCommits(t *testing.T) {
	lines := []string{
		"o  9e78f1f32d  Today at 04:31  remote/master",
		"╷",
		"╷ o  a370877256  Today at 04:34  meriksen",
		"╷ │  [hyperactor] tighten parser cleanup and regression coverage",
		"╷ │",
		"╷ @  90c63bd2b4  Today at 04:34  meriksen  D101700572",
		"╷ │  [hyperactor] begin internal migration from reference:: to ref_:: types",
		"╷ │",
	}

	commits := parseCommits(makeSmartlogLines(lines))
	if len(commits) != 3 {
		t.Fatalf("got %d commits, want 3", len(commits))
	}

	if commits[1].Hash != "a370877256" {
		t.Fatalf("got hash %q", commits[1].Hash)
	}
	if commits[2].DiffID != "D101700572" {
		t.Fatalf("got diff ID %q", commits[2].DiffID)
	}
	if commits[1].SubjectLine != 3 {
		t.Fatalf("got subject line %d, want 3", commits[1].SubjectLine)
	}
	if commits[1].BodyPrefix != "╷ │  " {
		t.Fatalf("got prefix %q", commits[1].BodyPrefix)
	}
	if commits[1].ExpandPrefix != "╷ │  " {
		t.Fatalf("got expand prefix %q", commits[1].ExpandPrefix)
	}
}

func TestParseCommitsWithAmendedAnnotation(t *testing.T) {
	lines := []string{
		"o  9e78f1f32d  Today at 04:31  remote/master",
		"╷",
		"╷ @  eeddf4df4e [Amended as b09aa43f2ff0] (Backup pending)  3 minutes ago  meriksen  D102621986  (stale phab)",
		"╷ │  [hyperactor] rewrite id parsing to use shared parser",
	}

	commits := parseCommits(makeSmartlogLines(lines))
	if len(commits) != 2 {
		t.Fatalf("got %d commits, want 2", len(commits))
	}
	if commits[1].Marker != "@" {
		t.Fatalf("got marker %q, want @", commits[1].Marker)
	}
	if commits[1].Hash != "eeddf4df4e" {
		t.Fatalf("got hash %q, want eeddf4df4e", commits[1].Hash)
	}
}

func TestRenderExpansionBody(t *testing.T) {
	c := commit{
		SubjectText:  "subject",
		BodyPrefix:   "│  ",
		ExpandPrefix: "│  ",
		Description:  "subject\nbody one\n\nbody two\n",
	}

	got := renderExpansionBody(c, 80, md.RenderStyle{})
	want := []smartlogLine{
		{raw: "│", plain: "│"},
		{raw: "│  body one", plain: "│  body one"},
		{raw: "│", plain: "│"},
		{raw: "│  body two", plain: "│  body two"},
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v want %#v", got, want)
	}
}

func TestSplitContentLinePreservesIndentedGraphPrefix(t *testing.T) {
	prefix, content, ok := splitContentLine("  │  [hyperactor] export true id/addr/ref names")
	if !ok {
		t.Fatal("expected content line")
	}
	if prefix != "  │  " {
		t.Fatalf("got prefix %q want %q", prefix, "  │  ")
	}
	if content != "[hyperactor] export true id/addr/ref names" {
		t.Fatalf("got content %q", content)
	}
}

func TestParseCommitsDerivesExpandPrefixFromGraphOnlyLine(t *testing.T) {
	lines := []string{
		"o  3c5110e0ee  21 minutes ago  remote/master",
		"╷",
		"╷ @  050970ae43  14 seconds ago  meriksen",
		"╭─╯  Add clap to hyperactor_mesh Cargo test dependencies",
		"│",
		"o  05b073be77  Today at 06:01  remote/fbcode/stable",
	}

	commits := parseCommits(makeSmartlogLines(lines))
	if len(commits) != 3 {
		t.Fatalf("got %d commits, want 3", len(commits))
	}
	if commits[1].BodyPrefix != "╭─╯  " {
		t.Fatalf("got body prefix %q", commits[1].BodyPrefix)
	}
	if commits[1].ExpandPrefix != "│    " {
		t.Fatalf("got expand prefix %q, want %q", commits[1].ExpandPrefix, "│    ")
	}
}

func TestRenderExpansionBodyUsesExpandPrefix(t *testing.T) {
	c := commit{
		SubjectText:  "subject",
		BodyPrefix:   "╭─╯  ",
		ExpandPrefix: "│    ",
		Description:  "subject\nSummary: body one\n\nTest Plan: body two\n",
	}

	got := renderExpansionBody(c, 80, md.RenderStyle{})
	want := []smartlogLine{
		{raw: "│", plain: "│"},
		{raw: "│    Summary: body one", plain: "│    Summary: body one"},
		{raw: "│", plain: "│"},
		{raw: "│    Test Plan: body two", plain: "│    Test Plan: body two"},
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v want %#v", got, want)
	}
}

func TestRenderExpansionBodyWrapsWithinExpandPrefix(t *testing.T) {
	c := commit{
		SubjectText:  "subject",
		BodyPrefix:   "╭─╯  ",
		ExpandPrefix: "│    ",
		Description:  "subject\nSummary: Remove the remaining root-level hyperactor type renames so the crate exports the real addr, id, and ref_ names directly, and retarget downstream users to those true names. This is a mechanical rename only.\n",
	}

	got := renderExpansionBody(c, 60, md.RenderStyle{})
	if len(got) < 3 {
		t.Fatalf("got %#v, want wrapped output", got)
	}

	blankPrefix := "│"
	for _, line := range got {
		if line.plain == blankPrefix {
			continue
		}
		if len(line.plain) <= len(c.ExpandPrefix) || line.plain[:len(c.ExpandPrefix)] != c.ExpandPrefix {
			t.Fatalf("line %q missing expand prefix %q", line.plain, c.ExpandPrefix)
		}
	}
}

func TestExpansionRenderWidthPrefersExpandPrefix(t *testing.T) {
	c := commit{
		BodyPrefix:   "│  ",
		ExpandPrefix: "│    ",
	}

	want := terminalWidth() - displayWidth(c.ExpandPrefix)
	if want > 100 {
		want = 100
	}
	if want < 20 {
		want = 20
	}

	if got := expansionRenderWidth(c); got != want {
		t.Fatalf("got %d want %d", got, want)
	}
}

func TestFormatRenderedLine(t *testing.T) {
	if got := formatRenderedLine("plain", false, lineStyle{}); got != "plain" {
		t.Fatalf("got %q want %q", got, "plain")
	}

	if got := formatRenderedLine("selected", true, lineStyle{}); got != "\x1b[1mselected\x1b[0m" {
		t.Fatalf("got %q", got)
	}

	style := lineStyle{start: "\x1b[41m", end: "\x1b[0m"}
	if got := formatRenderedLine("selected", true, style); got != "\x1b[41mselected\x1b[0m" {
		t.Fatalf("got %q", got)
	}
}

func TestReadKeyMapsCtrlR(t *testing.T) {
	got, err := readKey(bufio.NewReader(strings.NewReader("\x12")), 0)
	if err != nil {
		t.Fatal(err)
	}
	if got != keyCtrlR {
		t.Fatalf("got %v want %v", got, keyCtrlR)
	}
}

func TestWaitForInputRetriesInterruptedSystemCall(t *testing.T) {
	oldSelectInput := selectInput
	defer func() {
		selectInput = oldSelectInput
	}()

	calls := 0
	selectInput = func(nfd int, r, w, e *syscall.FdSet, timeout *syscall.Timeval) (int, error) {
		calls++
		if calls == 1 {
			return 0, syscall.EINTR
		}
		return 1, nil
	}

	ok, err := waitForInput(0, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("got no input after interrupted select retry")
	}
	if calls != 2 {
		t.Fatalf("got %d select calls, want 2", calls)
	}
}

func TestAdjustViewportTopPreservesViewport(t *testing.T) {
	lineRows := []int{1, 1, 1, 1, 1, 1}
	if got := adjustViewportTop(3, 0, lineRows, 3, true); got != 3 {
		t.Fatalf("got %d want %d", got, 3)
	}
}

func TestAdjustViewportTopTracksSelection(t *testing.T) {
	lineRows := []int{1, 1, 1, 1, 1, 1}
	if got := adjustViewportTop(3, 0, lineRows, 3, false); got != 0 {
		t.Fatalf("got %d want %d", got, 0)
	}
}

func TestBuildRenderedLinesIncludesExpansion(t *testing.T) {
	m := &model{
		lines: makeSmartlogLines([]string{
			"o  aaaaaaaaaa  now",
			"│  subject",
			"│",
			"o  bbbbbbbbbb  now",
			"│  second",
		}),
		commits: []commit{
			{
				Hash:        "aaaaaaaaaa",
				HeaderLine:  0,
				AnchorLine:  1,
				SubjectLine: 1,
				BodyLines:   []smartlogLine{{raw: "│  details", plain: "│  details"}},
			},
			{
				Hash:        "bbbbbbbbbb",
				HeaderLine:  3,
				AnchorLine:  4,
				SubjectLine: 4,
			},
		},
		selected: 1,
		expanded: map[string]bool{"aaaaaaaaaa": true},
	}

	got, selected := buildRenderedLines(m)
	want := []smartlogLine{
		{raw: "o  aaaaaaaaaa  now", plain: "o  aaaaaaaaaa  now"},
		{raw: "│  subject", plain: "│  subject"},
		{raw: "│  details", plain: "│  details"},
		{raw: "│", plain: "│"},
		{raw: "o  bbbbbbbbbb  now", plain: "o  bbbbbbbbbb  now"},
		{raw: "│  second", plain: "│  second"},
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v want %#v", got, want)
	}
	if selected != 4 {
		t.Fatalf("got selected line %d want 4", selected)
	}
}

func TestBuildRenderedLinesAppendsStatusAtBottom(t *testing.T) {
	m := &model{
		lines: makeSmartlogLines([]string{
			"@  aaaaaaaaaa  now",
			"│  subject",
		}),
		statusLines: makeSmartlogLines([]string{
			"Changes not committed:",
			"M main.go",
		}),
		commits: []commit{{
			Hash:       "aaaaaaaaaa",
			HeaderLine: 0,
			AnchorLine: 1,
		}},
		expanded: map[string]bool{},
	}

	got, _ := buildRenderedLines(m)
	want := []string{
		"@  aaaaaaaaaa  now",
		"│  subject",
		"",
		"Changes not committed:",
		"M main.go",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d lines, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].plain != want[i] {
			t.Fatalf("line %d: got %q, want %q", i, got[i].plain, want[i])
		}
	}
}

func TestBuildRenderedLinesDoesNotAppendSeparatorForCleanStatus(t *testing.T) {
	m := &model{
		lines:    makeSmartlogLines([]string{"@  aaaaaaaaaa  now"}),
		commits:  []commit{{Hash: "aaaaaaaaaa", HeaderLine: 0, AnchorLine: 0}},
		expanded: map[string]bool{},
	}

	got, _ := buildRenderedLines(m)
	if len(got) != 1 || got[0].plain != "@  aaaaaaaaaa  now" {
		t.Fatalf("got %#v", got)
	}
}

func TestStatusBlockIsSelectableWithArrowNavigation(t *testing.T) {
	m := &model{
		statusLines:  makeSmartlogLines([]string{"M main.go", "? notes.txt"}),
		commits:      []commit{{Hash: "aaaaaaaaaa"}, {Hash: "bbbbbbbbbb"}},
		selected:     1,
		selectedHash: "bbbbbbbbbb",
	}

	moveSelectionDown(m)
	if !m.statusSelected {
		t.Fatal("Down did not select status block")
	}
	moveSelectionDown(m)
	if !m.statusSelected {
		t.Fatal("second Down left status block")
	}
	moveSelectionUp(m)
	if m.statusSelected || m.selected != 1 {
		t.Fatalf("Up returned status=%v commit=%d", m.statusSelected, m.selected)
	}
}

func TestBuildRenderedLinesSelectsStatusBlockStart(t *testing.T) {
	m := &model{
		lines:          makeSmartlogLines([]string{"@  aaaaaaaaaa  now"}),
		statusLines:    makeSmartlogLines([]string{"M main.go", "? notes.txt"}),
		commits:        []commit{{Hash: "aaaaaaaaaa", HeaderLine: 0, AnchorLine: 0}},
		expanded:       map[string]bool{},
		statusSelected: true,
	}

	_, selectedLine := buildRenderedLines(m)
	if selectedLine != 2 {
		t.Fatalf("selected line = %d, want 2", selectedLine)
	}
}

func TestRenderedLineSelectedIncludesEntireStatusBlock(t *testing.T) {
	m := &model{statusSelected: true}
	if renderedLineSelected(m, 1, 2, 2) {
		t.Fatal("separator before status block was selected")
	}
	if !renderedLineSelected(m, 2, 2, 2) || !renderedLineSelected(m, 3, 2, 2) {
		t.Fatal("not every status line was selected")
	}
}

func TestMdiffArgsUsesWorkingCopyForStatusBlock(t *testing.T) {
	m := &model{
		commits:        []commit{{Hash: "aaaaaaaaaa"}},
		statusSelected: true,
	}
	if got := mdiffArgs(m); got != nil {
		t.Fatalf("got %#v, want no arguments", got)
	}
}

func TestMdiffArgsUsesCommitForCommitSelection(t *testing.T) {
	m := &model{commits: []commit{{Hash: "aaaaaaaaaa"}}}
	want := []string{"-c", "aaaaaaaaaa"}
	if got := mdiffArgs(m); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestBuildRenderedLinesShowsPhabStatus(t *testing.T) {
	m := &model{
		lines: makeSmartlogLines([]string{
			"@  aaaaaaaaaa  now  D123",
			"│  subject",
		}),
		commits: []commit{
			{
				Hash:       "aaaaaaaaaa",
				HeaderLine: 0,
				AnchorLine: 0,
				DiffID:     "D123",
			},
		},
		expanded:     map[string]bool{},
		phabStatuses: map[string]string{"D123": "landed"},
	}

	got, _ := buildRenderedLines(m)
	if got[0].plain != "@  aaaaaaaaaa  now  D123  landed" {
		t.Fatalf("got %q", got[0].plain)
	}
}

func TestBuildRenderedLinesAddsKaleidoscopeLinkForDiff(t *testing.T) {
	const fullHash = "d515a67992a23a75a06a01d5f969ea346228fa17"
	m := &model{
		lines: makeSmartlogLines([]string{
			"@  d515a67992  now  D123",
			"│  subject",
		}),
		commits: []commit{
			{
				Hash:       "d515a67992",
				FullHash:   fullHash,
				HeaderLine: 0,
				AnchorLine: 0,
				DiffID:     "D123",
			},
		},
		expanded:     map[string]bool{},
		kaleidoscope: true,
	}

	got, _ := buildRenderedLines(m)
	if got[0].plain != "@  d515a67992  now  D123  ksdiff" {
		t.Fatalf("got plain line %q", got[0].plain)
	}
	wantLink := "\x1b]8;;ksdiff://" + fullHash + "\x1b\\\x1b[1mksdiff\x1b[22m\x1b]8;;\x1b\\"
	if !strings.Contains(got[0].raw, wantLink) {
		t.Fatalf("raw line %q does not contain %q", got[0].raw, wantLink)
	}
}

func TestAssignFullHashesOnlyMatchesDraftCommits(t *testing.T) {
	commits := []commit{
		{Hash: "1111111111"},
		{Hash: "2222222222"},
	}
	assignFullHashes(commits, []string{"2222222222222222222222222222222222222222"})

	if commits[0].FullHash != "" {
		t.Fatalf("got full hash %q for non-draft commit", commits[0].FullHash)
	}
	if commits[1].FullHash != "2222222222222222222222222222222222222222" {
		t.Fatalf("got full hash %q", commits[1].FullHash)
	}
}

func TestBuildRenderedLinesOmitsKaleidoscopeLinkOutsideFBSource(t *testing.T) {
	m := &model{
		lines: makeSmartlogLines([]string{"@  d515a67992  now  D123"}),
		commits: []commit{{
			Hash:       "d515a67992",
			FullHash:   "d515a67992a23a75a06a01d5f969ea346228fa17",
			HeaderLine: 0,
			AnchorLine: 0,
			DiffID:     "D123",
		}},
		expanded: map[string]bool{},
	}

	got, _ := buildRenderedLines(m)
	if got[0].plain != "@  d515a67992  now  D123" {
		t.Fatalf("got %q", got[0].plain)
	}
}

func TestBuildRenderedLinesShowsPendingPhabSpinner(t *testing.T) {
	m := &model{
		lines: makeSmartlogLines([]string{
			"@  aaaaaaaaaa  now  D123",
			"│  subject",
		}),
		commits: []commit{
			{
				Hash:       "aaaaaaaaaa",
				HeaderLine: 0,
				AnchorLine: 0,
				DiffID:     "D123",
			},
		},
		expanded:      map[string]bool{},
		phabPending:   map[string]bool{"D123": true},
		progressFrame: 1,
	}

	got, _ := buildRenderedLines(m)
	if got[0].plain != "@  aaaaaaaaaa  now  D123  ⠙" {
		t.Fatalf("got %q", got[0].plain)
	}
}

func TestBuildRenderedLinesCanHidePendingPhabSpinner(t *testing.T) {
	m := &model{
		lines: makeSmartlogLines([]string{
			"@  aaaaaaaaaa  now  D123",
			"│  subject",
		}),
		commits: []commit{
			{
				Hash:       "aaaaaaaaaa",
				HeaderLine: 0,
				AnchorLine: 0,
				DiffID:     "D123",
			},
		},
		expanded:      map[string]bool{},
		phabPending:   map[string]bool{"D123": true},
		progressFrame: 1,
	}

	got, _ := buildRenderedLinesWithPending(m, false)
	if got[0].plain != "@  aaaaaaaaaa  now  D123" {
		t.Fatalf("got %q", got[0].plain)
	}
}

func TestPhabStatusFromMapPrefersLanded(t *testing.T) {
	got := phabStatusFromMap(map[string]any{
		"status":    "Closed",
		"is_landed": "true",
	})
	if got != "landed" {
		t.Fatalf("got %q want landed", got)
	}
}

func TestPhabStatusFromMapFallsBackToHumanStatus(t *testing.T) {
	got := phabStatusFromMap(map[string]any{
		"status": "NEEDS_REVIEW",
	})
	if got != "needs review" {
		t.Fatalf("got %q want needs review", got)
	}
}

func TestDisplayRows(t *testing.T) {
	cases := []struct {
		line  string
		width int
		want  int
	}{
		{line: "", width: 10, want: 1},
		{line: "short", width: 10, want: 1},
		{line: "1234567890", width: 10, want: 1},
		{line: "12345678901", width: 10, want: 2},
		{line: "o  3c5110e0ee  25 minutes ago  remote/master remote/fbcode/stable", width: 40, want: 2},
	}

	for _, tc := range cases {
		if got := displayRows(tc.line, tc.width); got != tc.want {
			t.Fatalf("displayRows(%q, %d) = %d, want %d", tc.line, tc.width, got, tc.want)
		}
	}
}
