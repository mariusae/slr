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
		SubjectText: "subject",
		BodyPrefix:  "│  ",
		Description: "subject\nbody one\n\nbody two\n",
	}

	got := renderExpansionBody(c, 80, md.RenderStyle{})
	want := []smartlogLine{
		{raw: "│  body one", plain: "│  body one"},
		{raw: "│", plain: "│"},
		{raw: "│  body two", plain: "│  body two"},
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v want %#v", got, want)
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
