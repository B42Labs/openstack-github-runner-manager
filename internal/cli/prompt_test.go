// SPDX-FileCopyrightText: Copyright 2026 B42Labs contributors
// SPDX-License-Identifier: BUSL-1.1

package cli

import (
	"errors"
	"fmt"
	"testing"
)

// scriptedAsker replays canned answers in order and records the labels it was
// asked with, so a test can assert what was prompted.
type scriptedAsker struct {
	answers []string
	calls   int
	labels  []string
}

func (s *scriptedAsker) ask(label string) (string, error) {
	s.labels = append(s.labels, label)
	if s.calls >= len(s.answers) {
		return "", errors.New("no more scripted answers")
	}
	a := s.answers[s.calls]
	s.calls++
	return a, nil
}

// stubMint records the repo URLs it was asked to mint for and returns a
// deterministic token per call, so a test can assert the auto-mint path.
type stubMint struct {
	repos []string
}

func (m *stubMint) mint(repoURL string) (string, error) {
	m.repos = append(m.repos, repoURL)
	return fmt.Sprintf("minted-%d", len(m.repos)), nil
}

// TestResolveMintsOneTokenPerNewInstance proves the default path: no -token
// flags, repo prompted once, one mint per new instance against that repo.
func TestResolveMintsOneTokenPerNewInstance(t *testing.T) {
	s := &scriptedAsker{answers: []string{"https://github.com/acme/example"}}
	m := &stubMint{}
	repo, tokens, err := resolveRepoAndTokens("", nil, []int{3, 4}, s.ask, m.mint)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if repo != "https://github.com/acme/example" {
		t.Errorf("repo = %q", repo)
	}
	// One mint per new instance, each against the resolved repo.
	if len(m.repos) != 2 || m.repos[0] != repo || m.repos[1] != repo {
		t.Errorf("mint calls = %v; want two for %q", m.repos, repo)
	}
	if len(tokens) != 2 || tokens[0] != "minted-1" || tokens[1] != "minted-2" {
		t.Errorf("tokens = %v; want [minted-1 minted-2]", tokens)
	}
	// The only prompt is the repository URL; there is no per-token prompt any more.
	if s.calls != 1 {
		t.Errorf("expected exactly one prompt (the repo URL), got %d", s.calls)
	}
}

func TestResolveUsesFlagsWithoutMintingOrPrompting(t *testing.T) {
	s := &scriptedAsker{} // no answers; any prompt would error
	repo, tokens, err := resolveRepoAndTokens("https://github.com/acme/example", []string{"a", "b"}, []int{1, 2}, s.ask, mustNotMint(t))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if s.calls != 0 {
		t.Errorf("expected no prompts, got %d", s.calls)
	}
	if repo != "https://github.com/acme/example" || len(tokens) != 2 || tokens[0] != "a" || tokens[1] != "b" {
		t.Errorf("repo=%q tokens=%v", repo, tokens)
	}
}

func TestResolveRejectsPartialTokenList(t *testing.T) {
	// One token for two new instances is now an error: tokens are all-or-nothing
	// so the source of each is unambiguous (mint them all, or supply them all).
	_, _, err := resolveRepoAndTokens("https://github.com/acme/example", []string{"tok-1"}, []int{1, 2}, (&scriptedAsker{}).ask, mustNotMint(t))
	if err == nil {
		t.Fatal("expected an error for fewer tokens than instances")
	}
}

func TestResolveRejectsTooManyTokens(t *testing.T) {
	_, _, err := resolveRepoAndTokens("https://github.com/acme/example", []string{"a", "b", "c"}, []int{1, 2}, (&scriptedAsker{}).ask, mustNotMint(t))
	if err == nil {
		t.Fatalf("expected an error for more tokens than instances")
	}
}

func TestResolveRejectsEmptyMintedToken(t *testing.T) {
	emptyMint := func(string) (string, error) { return "   ", nil }
	_, _, err := resolveRepoAndTokens("https://github.com/acme/example", nil, []int{1}, (&scriptedAsker{}).ask, emptyMint)
	if err == nil {
		t.Fatalf("expected an error for an empty minted token")
	}
}

func TestResolveRejectsInvalidRepoBeforeMinting(t *testing.T) {
	// A bad repo URL must fail before any mint happens.
	_, _, err := resolveRepoAndTokens("not-a-url", nil, []int{1}, (&scriptedAsker{}).ask, mustNotMint(t))
	if err == nil {
		t.Fatalf("expected an error for an invalid repository URL")
	}
}

func TestConfirm(t *testing.T) {
	cases := map[string]bool{"y": true, "yes": true, "YES": true, "n": false, "": false, "nope": false}
	for answer, want := range cases {
		s := &scriptedAsker{answers: []string{answer}}
		got, err := confirm(s.ask, "Proceed?")
		if err != nil {
			t.Fatalf("confirm(%q): %v", answer, err)
		}
		if got != want {
			t.Errorf("confirm(%q) = %v; want %v", answer, got, want)
		}
	}
}

func TestSplitCSV(t *testing.T) {
	got := splitCSV(" 9.9.9.9 , , 1.1.1.1 ")
	if len(got) != 2 || got[0] != "9.9.9.9" || got[1] != "1.1.1.1" {
		t.Errorf("splitCSV = %v", got)
	}
}
