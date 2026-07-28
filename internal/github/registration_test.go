// SPDX-FileCopyrightText: Copyright 2026 B42Labs contributors
// SPDX-License-Identifier: BUSL-1.1

package github

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestRegistrationTokenPath(t *testing.T) {
	cases := []struct {
		name    string
		url     string
		want    string
		wantErr bool
	}{
		{"repo", "https://github.com/acme/example", "/repos/acme/example/actions/runners/registration-token", false},
		{"org", "https://github.com/acme", "/orgs/acme/actions/runners/registration-token", false},
		{"trailing slash repo", "https://github.com/acme/widgets/", "/repos/acme/widgets/actions/runners/registration-token", false},
		{"too many segments", "https://github.com/acme/widgets/extra", "", true},
		{"no path", "https://github.com/", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := registrationTokenPath(tc.url)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error for %q", tc.url)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("path = %q; want %q", got, tc.want)
			}
		})
	}
}

// TestMintRegistrationTokenInvokesGhAndTrims proves the happy path: the client
// calls `gh api` against the derived repo path and returns gh's stdout trimmed.
func TestMintRegistrationTokenInvokesGhAndTrims(t *testing.T) {
	var gotArgs []string
	c := NewClientWithRunner(func(_ context.Context, name string, args ...string) (string, error) {
		gotArgs = append([]string{name}, args...)
		return "AABBCC\n", nil
	})

	tok, err := c.MintRegistrationToken(context.Background(), "https://github.com/acme/example")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if tok != "AABBCC" {
		t.Errorf("token = %q; want trimmed AABBCC", tok)
	}
	want := []string{"gh", "api", "--method", "POST", "/repos/acme/example/actions/runners/registration-token", "--jq", ".token"}
	if strings.Join(gotArgs, " ") != strings.Join(want, " ") {
		t.Errorf("gh invoked with %v; want %v", gotArgs, want)
	}
}

func TestMintRegistrationTokenRejectsEmpty(t *testing.T) {
	c := NewClientWithRunner(func(_ context.Context, _ string, _ ...string) (string, error) {
		return "   \n", nil
	})
	if _, err := c.MintRegistrationToken(context.Background(), "https://github.com/acme/widgets"); err == nil {
		t.Fatal("expected an error for an empty token")
	}
}

func TestMintRegistrationTokenPropagatesRunnerError(t *testing.T) {
	c := NewClientWithRunner(func(_ context.Context, _ string, _ ...string) (string, error) {
		return "", errors.New("gh: not authenticated")
	})
	_, err := c.MintRegistrationToken(context.Background(), "https://github.com/acme/widgets")
	if err == nil {
		t.Fatal("expected the runner error to propagate")
	}
	if !strings.Contains(err.Error(), "not authenticated") {
		t.Errorf("error should mention the gh failure: %v", err)
	}
}

func TestMintRegistrationTokenRejectsBadURL(t *testing.T) {
	c := NewClientWithRunner(func(_ context.Context, _ string, _ ...string) (string, error) {
		t.Fatal("runner must not be called for an invalid URL")
		return "", nil
	})
	if _, err := c.MintRegistrationToken(context.Background(), "https://github.com/"); err == nil {
		t.Fatal("expected an error for a URL with no org/repo")
	}
}
