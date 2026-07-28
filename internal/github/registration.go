// SPDX-FileCopyrightText: Copyright 2026 B42Labs contributors
// SPDX-License-Identifier: BUSL-1.1

// Package github is a thin anti-corruption layer over the GitHub CLI (`gh`).
// Its single job is to mint the short-lived registration token a self-hosted
// runner needs to call config.sh, so an operator no longer fetches each token
// by hand from Settings -> Actions -> Runners. It shells out to `gh api`,
// reusing the operator's existing `gh auth login` session rather than handling
// a personal access token itself.
//
// The package deliberately knows nothing about OpenStack, cloud-init, or the
// fleet: it maps a GitHub org or org/repo URL to the matching
// actions/runners/registration-token endpoint and returns the token string.
package github

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os/exec"
	"strings"
)

// CommandRunner runs an external command and returns its stdout. It is the one
// seam the package is tested through: production wires it to execRunner, tests
// substitute a stub so neither a real `gh` binary nor a network call is needed.
type CommandRunner func(ctx context.Context, name string, args ...string) (string, error)

// Client mints runner registration tokens through the GitHub CLI.
type Client struct {
	run CommandRunner
}

// NewClient returns a Client that shells out to the real `gh` binary on PATH.
func NewClient() *Client {
	return &Client{run: execRunner}
}

// NewClientWithRunner returns a Client backed by a caller-supplied runner. It
// exists for tests, which inject a stub in place of the `gh` exec.
func NewClientWithRunner(run CommandRunner) *Client {
	return &Client{run: run}
}

// MintRegistrationToken returns a fresh runner registration token for the org
// or org/repo named by repoURL. The token authorises config.sh to register a
// runner and expires within roughly an hour, so it is meant to be spent right
// away by the instance this fleet is about to boot.
func (c *Client) MintRegistrationToken(ctx context.Context, repoURL string) (string, error) {
	apiPath, err := registrationTokenPath(repoURL)
	if err != nil {
		return "", err
	}
	// --jq .token narrows gh's JSON response to the bare token, so nothing but
	// the secret crosses the process boundary and the caller needs no parser.
	out, err := c.run(ctx, "gh", "api", "--method", "POST", apiPath, "--jq", ".token")
	if err != nil {
		return "", fmt.Errorf("mint registration token via gh: %w", err)
	}
	token := strings.TrimSpace(out)
	if token == "" {
		return "", fmt.Errorf("gh returned an empty registration token for %q", repoURL)
	}
	return token, nil
}

// registrationTokenPath maps a GitHub URL to the REST path that mints a runner
// registration token. A single path segment names an organization, two name an
// owner/repo; anything else is not a runner-bearing scope.
//
// DECISION: derive the scope from the URL the fleet already targets rather than
// taking a separate --org/--repo flag. The runner registers against exactly
// this URL (install.sh passes it to config.sh as --url), so a second source of
// truth could drift from it; reading both from one value keeps them in lockstep.
func registrationTokenPath(repoURL string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(repoURL))
	if err != nil {
		return "", fmt.Errorf("repository URL %q is not valid: %w", repoURL, err)
	}
	segments := strings.Split(strings.Trim(u.Path, "/"), "/")
	// strings.Split never returns an empty slice; an empty path yields [""], so
	// the segment-count switch below treats it as "no org/repo named".
	switch {
	case len(segments) == 1 && segments[0] != "":
		return fmt.Sprintf("/orgs/%s/actions/runners/registration-token", segments[0]), nil
	case len(segments) == 2 && segments[0] != "" && segments[1] != "":
		return fmt.Sprintf("/repos/%s/%s/actions/runners/registration-token", segments[0], segments[1]), nil
	default:
		return "", fmt.Errorf("repository URL %q must name an org or org/repo to mint a runner token", repoURL)
	}
}

// execRunner runs name with args and returns its stdout. On failure it folds
// the command's stderr into the error so a gh auth or permission problem is
// legible, while keeping stdout (which carries the token) out of the message.
func execRunner(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("%s: %w: %s", name, err, msg)
		}
		return "", fmt.Errorf("%s: %w", name, err)
	}
	return stdout.String(), nil
}
