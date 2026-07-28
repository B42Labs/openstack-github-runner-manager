// SPDX-FileCopyrightText: Copyright 2026 B42Labs contributors
// SPDX-License-Identifier: BUSL-1.1

package cli

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	"github.com/b42labs/openstack-github-runner-manager/internal/config"
)

// askFunc prompts for a single value and returns the trimmed line. It is an
// injectable seam so the resolution logic below can be unit-tested without a
// TTY.
type askFunc func(label string) (string, error)

// newAsker returns an askFunc backed by in/out: it prints the label and reads
// one line. The tool no longer prompts for secrets — registration tokens are
// minted through the GitHub CLI or passed with -token — so there is no no-echo
// path to special-case.
func newAsker(in io.Reader, out io.Writer) askFunc {
	reader := bufio.NewReader(in)
	return func(label string) (string, error) {
		fmt.Fprint(out, label)
		line, err := reader.ReadString('\n')
		if err != nil && line == "" {
			return "", err
		}
		return strings.TrimSpace(line), nil
	}
}

// mintFunc mints a registration token for the given org or org/repo URL. It is
// the seam the auto-token path is driven through: production binds it to the
// GitHub CLI client, tests substitute a stub.
type mintFunc func(repoURL string) (string, error)

// resolveRepoAndTokens fills the repository URL and one registration token per
// instance that the reconcile run will create — identified by createIndexes,
// the 1-based counters of the instances to add. The repository comes from the
// -repo flag or, when absent, an interactive prompt.
//
// Tokens are resolved one of two ways, never a mix: if the operator passed
// -token flags they are used verbatim and must line up one per new instance;
// otherwise a fresh token is minted per new instance via mint, so the operator
// never fetches one by hand. The returned tokens are positionally aligned with
// createIndexes, so tokens[k] belongs to instance createIndexes[k].
func resolveRepoAndTokens(repoFlag string, tokenFlags []string, createIndexes []int, ask askFunc, mint mintFunc) (string, []string, error) {
	repo := strings.TrimSpace(repoFlag)
	if repo == "" {
		v, err := ask("GitHub repository URL (e.g. https://github.com/acme/example): ")
		if err != nil {
			return "", nil, fmt.Errorf("read repository URL: %w", err)
		}
		repo = strings.TrimSpace(v)
	}
	// Validate here so both token paths fail fast on a bad URL — the mint path
	// also derives the GitHub API scope (org vs repo) from it.
	if err := config.ValidateRepoURL(repo); err != nil {
		return "", nil, err
	}

	// Explicit -token flags put the operator in full control (e.g. no gh on the
	// host, or org tokens minted elsewhere). They must cover every new instance;
	// a partial list is rejected rather than half-minted, so the source of each
	// token stays unambiguous.
	explicit := make([]string, 0, len(tokenFlags))
	for _, t := range tokenFlags {
		if s := strings.TrimSpace(t); s != "" {
			explicit = append(explicit, s)
		}
	}
	if len(explicit) > 0 {
		if len(explicit) != len(createIndexes) {
			return "", nil, fmt.Errorf("got %d -token value(s) but %d instance(s) to create; pass one -token per new instance, or none to mint them automatically", len(explicit), len(createIndexes))
		}
		return repo, explicit, nil
	}

	// No tokens supplied: mint one per new instance through the GitHub CLI. A
	// token per instance is correct whether or not a registration token is
	// single-use, and it keeps the token→index mapping the same as the explicit
	// path.
	tokens := make([]string, 0, len(createIndexes))
	for range createIndexes {
		tok, err := mint(repo)
		if err != nil {
			return "", nil, fmt.Errorf("mint registration token (or pass -token to supply your own): %w", err)
		}
		if tok = strings.TrimSpace(tok); tok == "" {
			return "", nil, fmt.Errorf("minted registration token is empty")
		}
		tokens = append(tokens, tok)
	}
	return repo, tokens, nil
}

// confirm asks a yes/no question, defaulting to no. It shares ask so it draws
// from the same input stream as the other prompts.
func confirm(ask askFunc, question string) (bool, error) {
	v, err := ask(question + " [y/N]: ")
	if err != nil {
		return false, err
	}
	v = strings.ToLower(strings.TrimSpace(v))
	return v == "y" || v == "yes", nil
}

// splitCSV splits a comma-separated flag value into trimmed, non-empty parts.
func splitCSV(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}
