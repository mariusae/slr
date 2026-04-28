package main

import (
	"reflect"
	"testing"

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
