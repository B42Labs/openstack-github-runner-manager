// SPDX-FileCopyrightText: Copyright 2026 B42Labs contributors
// SPDX-License-Identifier: BUSL-1.1

package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Runner is a self-hosted runner as GitHub reports it.
type Runner struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"` // "online" or "offline"
	Busy   bool   `json:"busy"`   // running a job right now
}

// ListRunners returns every self-hosted runner registered against the org or
// org/repo named by repoURL, across all result pages.
func (c *Client) ListRunners(ctx context.Context, repoURL string) ([]Runner, error) {
	apiPath, err := runnersPath(repoURL)
	if err != nil {
		return nil, err
	}
	// --paginate follows GitHub's Link headers and --jq runs on every page, so
	// the output is one runner object after another with no page wrapper left
	// to stitch together. A decoder reads that stream whether gh prints the
	// objects compact or indented.
	out, err := c.run(ctx, "gh", "api", "--paginate", apiPath+"?per_page=100", "--jq", ".runners[]")
	if err != nil {
		return nil, fmt.Errorf("list runners via gh: %w", err)
	}
	var runners []Runner
	dec := json.NewDecoder(strings.NewReader(out))
	for {
		var r Runner
		err := dec.Decode(&r)
		if errors.Is(err, io.EOF) {
			return runners, nil
		}
		if err != nil {
			return nil, fmt.Errorf("parse the runners gh listed for %q: %w", repoURL, err)
		}
		runners = append(runners, r)
	}
}

// DeleteRunner removes the self-hosted runner with the given id from the org or
// org/repo named by repoURL.
func (c *Client) DeleteRunner(ctx context.Context, repoURL string, id int64) error {
	apiPath, err := runnersPath(repoURL)
	if err != nil {
		return err
	}
	if _, err := c.run(ctx, "gh", "api", "--method", "DELETE", fmt.Sprintf("%s/%d", apiPath, id)); err != nil {
		return fmt.Errorf("delete runner %d via gh: %w", id, err)
	}
	return nil
}
