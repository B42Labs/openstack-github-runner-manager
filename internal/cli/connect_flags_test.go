// SPDX-FileCopyrightText: Copyright 2026 B42Labs contributors
// SPDX-License-Identifier: BUSL-1.1

package cli

import (
	"io"
	"testing"
	"time"

	"github.com/b42labs/openstack-github-runner-manager/internal/openstack"
)

func TestConnectFlagsDefaults(t *testing.T) {
	f, err := parseCreateFlags([]string{"-name", "acme"}, io.Discard)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if f.connect.timeout != openstack.DefaultConnectTimeout {
		t.Errorf("connect-timeout default = %s; want %s", f.connect.timeout, openstack.DefaultConnectTimeout)
	}
	if f.connect.attempts != openstack.DefaultConnectAttempts {
		t.Errorf("connect-attempts default = %d; want %d", f.connect.attempts, openstack.DefaultConnectAttempts)
	}
}

func TestConnectFlagsOverride(t *testing.T) {
	f, err := parseCreateFlags([]string{"-name", "acme", "-connect-timeout", "30s", "-connect-attempts", "5"}, io.Discard)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if f.connect.timeout != 30*time.Second {
		t.Errorf("connect-timeout = %s; want 30s", f.connect.timeout)
	}
	if f.connect.attempts != 5 {
		t.Errorf("connect-attempts = %d; want 5", f.connect.attempts)
	}
}

func TestConnectFlagsValidation(t *testing.T) {
	if _, err := parseCreateFlags([]string{"-name", "acme", "-connect-attempts", "0"}, io.Discard); err == nil {
		t.Error("expected an error for -connect-attempts 0")
	}
	if _, err := parseCreateFlags([]string{"-name", "acme", "-connect-timeout", "0s"}, io.Discard); err == nil {
		t.Error("expected an error for -connect-timeout 0s")
	}
	// The same guard must be wired on delete and list.
	if _, err := parseDeleteFlags([]string{"-name", "acme", "-connect-attempts", "-1"}, io.Discard); err == nil {
		t.Error("delete: expected an error for -connect-attempts -1")
	}
	if _, err := parseListFlags([]string{"-name", "acme", "-connect-timeout", "-5s"}, io.Discard); err == nil {
		t.Error("list: expected an error for -connect-timeout -5s")
	}
}
