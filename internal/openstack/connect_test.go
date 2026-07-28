// SPDX-FileCopyrightText: Copyright 2026 B42Labs contributors
// SPDX-License-Identifier: BUSL-1.1

package openstack

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// blockUntilDeadline waits for the attempt context's own deadline to fire and
// then returns its error, emulating a connection that hangs past the timeout.
func blockUntilDeadline(ctx context.Context) (*Clients, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestConnectWithRetrySucceedsFirstTry(t *testing.T) {
	calls := 0
	dial := func(context.Context) (*Clients, error) {
		calls++
		return &Clients{}, nil
	}
	c, err := connectWithRetry(context.Background(), 50*time.Millisecond, 3, io.Discard, dial)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c == nil {
		t.Fatal("expected clients, got nil")
	}
	if calls != 1 {
		t.Errorf("dial called %d times; want 1", calls)
	}
}

func TestConnectWithRetryRetriesOnTimeoutThenGivesUp(t *testing.T) {
	calls := 0
	dial := func(ctx context.Context) (*Clients, error) {
		calls++
		return blockUntilDeadline(ctx)
	}
	_, err := connectWithRetry(context.Background(), 10*time.Millisecond, 3, io.Discard, dial)
	if err == nil {
		t.Fatal("expected an error after exhausting attempts")
	}
	if calls != 3 {
		t.Errorf("dial called %d times; want 3 (one per attempt)", calls)
	}
	if !strings.Contains(err.Error(), "timed out after 3 attempt") {
		t.Errorf("error = %q; want it to mention timing out after 3 attempts", err.Error())
	}
}

func TestConnectWithRetrySucceedsAfterATimeout(t *testing.T) {
	calls := 0
	dial := func(ctx context.Context) (*Clients, error) {
		calls++
		if calls == 1 {
			return blockUntilDeadline(ctx)
		}
		return &Clients{}, nil
	}
	c, err := connectWithRetry(context.Background(), 10*time.Millisecond, 3, io.Discard, dial)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c == nil {
		t.Fatal("expected clients, got nil")
	}
	if calls != 2 {
		t.Errorf("dial called %d times; want 2 (timeout, then success)", calls)
	}
}

func TestConnectWithRetryDoesNotRetryNonTimeout(t *testing.T) {
	calls := 0
	boom := errors.New("invalid credentials")
	dial := func(context.Context) (*Clients, error) {
		calls++
		return nil, boom
	}
	_, err := connectWithRetry(context.Background(), 50*time.Millisecond, 3, io.Discard, dial)
	if err == nil {
		t.Fatal("expected an error")
	}
	if calls != 1 {
		t.Errorf("dial called %d times; want 1 (a non-timeout error must not be retried)", calls)
	}
	if !errors.Is(err, boom) {
		t.Errorf("error = %v; want it to wrap the underlying credential error", err)
	}
}

func TestConnectWithRetryStopsOnParentCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	dial := func(context.Context) (*Clients, error) {
		calls++
		return &Clients{}, nil
	}
	_, err := connectWithRetry(ctx, 10*time.Millisecond, 3, io.Discard, dial)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v; want context.Canceled", err)
	}
	if calls != 0 {
		t.Errorf("dial called %d times; want 0 (parent already cancelled)", calls)
	}
}

func TestConnectWithRetryClampsDefaults(t *testing.T) {
	calls := 0
	dial := func(context.Context) (*Clients, error) {
		calls++
		return &Clients{}, nil
	}
	// attempts < 1 must behave as a single attempt, timeout <= 0 must not
	// produce an immediately-expired context that fails a healthy dial.
	if _, err := connectWithRetry(context.Background(), 0, 0, io.Discard, dial); err != nil {
		t.Fatalf("unexpected error with clamped defaults: %v", err)
	}
	if calls != 1 {
		t.Errorf("dial called %d times; want 1", calls)
	}
}
