// SPDX-FileCopyrightText: Copyright 2026 B42Labs contributors
// SPDX-License-Identifier: BUSL-1.1

package github

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// TestListRunnersReadsEveryPage proves the listing path: gh is asked for every
// page of the repository's runner collection, and every runner object it
// streams back comes out parsed, whether gh printed it on one line or indented.
func TestListRunnersReadsEveryPage(t *testing.T) {
	var gotArgs []string
	c := NewClientWithRunner(func(_ context.Context, name string, args ...string) (string, error) {
		gotArgs = append([]string{name}, args...)
		return `{"id":1,"name":"ogrm-acme-001","status":"online","busy":false}
{"id":2,"name":"ogrm-acme-002","status":"offline","busy":false}
{
  "id": 3,
  "name": "ogrm-acme-003",
  "status": "online",
  "busy": true
}
`, nil
	})

	got, err := c.ListRunners(context.Background(), "https://github.com/acme/example")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	want := []Runner{
		{ID: 1, Name: "ogrm-acme-001", Status: "online"},
		{ID: 2, Name: "ogrm-acme-002", Status: "offline"},
		{ID: 3, Name: "ogrm-acme-003", Status: "online", Busy: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("runners = %+v; want %+v", got, want)
	}
	wantArgs := []string{"gh", "api", "--paginate", "/repos/acme/example/actions/runners?per_page=100", "--jq", ".runners[]"}
	if strings.Join(gotArgs, " ") != strings.Join(wantArgs, " ") {
		t.Errorf("gh invoked with %v; want %v", gotArgs, wantArgs)
	}
}

// A repository without runners is an empty result, not an error: jq emits
// nothing for an empty runners array.
func TestListRunnersWithNoneRegistered(t *testing.T) {
	c := NewClientWithRunner(func(_ context.Context, _ string, _ ...string) (string, error) {
		return "", nil
	})
	got, err := c.ListRunners(context.Background(), "https://github.com/acme/example")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("runners = %+v; want none", got)
	}
}

func TestListRunnersRejectsUnparseableOutput(t *testing.T) {
	c := NewClientWithRunner(func(_ context.Context, _ string, _ ...string) (string, error) {
		return "gh: something unexpected\n", nil
	})
	if _, err := c.ListRunners(context.Background(), "https://github.com/acme/example"); err == nil {
		t.Fatal("expected an error for output that is not runner JSON")
	}
}

func TestListRunnersPropagatesRunnerError(t *testing.T) {
	c := NewClientWithRunner(func(_ context.Context, _ string, _ ...string) (string, error) {
		return "", errors.New("gh: not authenticated")
	})
	_, err := c.ListRunners(context.Background(), "https://github.com/acme/example")
	if err == nil || !strings.Contains(err.Error(), "not authenticated") {
		t.Errorf("error should carry the gh failure: %v", err)
	}
}

// TestDeleteRunnerInvokesGh proves the deregistration addresses the one runner
// by id, here in an organization's runner collection.
func TestDeleteRunnerInvokesGh(t *testing.T) {
	var gotArgs []string
	c := NewClientWithRunner(func(_ context.Context, name string, args ...string) (string, error) {
		gotArgs = append([]string{name}, args...)
		return "", nil
	})
	if err := c.DeleteRunner(context.Background(), "https://github.com/acme", 42); err != nil {
		t.Fatalf("delete: %v", err)
	}
	want := []string{"gh", "api", "--method", "DELETE", "/orgs/acme/actions/runners/42"}
	if strings.Join(gotArgs, " ") != strings.Join(want, " ") {
		t.Errorf("gh invoked with %v; want %v", gotArgs, want)
	}
}

func TestDeleteRunnerPropagatesRunnerError(t *testing.T) {
	c := NewClientWithRunner(func(_ context.Context, _ string, _ ...string) (string, error) {
		return "", errors.New("gh: Validation Failed (HTTP 422)")
	})
	err := c.DeleteRunner(context.Background(), "https://github.com/acme/example", 42)
	if err == nil || !strings.Contains(err.Error(), "HTTP 422") {
		t.Errorf("error should carry the gh failure: %v", err)
	}
}
