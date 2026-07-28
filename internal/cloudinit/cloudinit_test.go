// SPDX-FileCopyrightText: Copyright 2026 B42Labs contributors
// SPDX-License-Identifier: BUSL-1.1

package cloudinit_test

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/b42labs/openstack-github-runner-manager/internal/cloudinit"
)

func goodParams() cloudinit.Params {
	return cloudinit.Params{
		RepoURL:            "https://github.com/acme/example",
		Token:              "TOKEN-A",
		RunnerName:         "ogrm-acme-001",
		InstallScript:      []byte("#!/usr/bin/env bash\necho hi\n"),
		DiskGuard:          true,
		DiskGuardThreshold: 80,
		DiskGuardInterval:  15 * time.Minute,
	}
}

func TestRenderProducesCloudConfigHeader(t *testing.T) {
	out, err := cloudinit.Render(goodParams())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.HasPrefix(string(out), "#cloud-config\n") {
		t.Fatalf("user-data must start with the #cloud-config header, got:\n%s", out)
	}
}

func TestRenderUpgradesPackages(t *testing.T) {
	out, _ := cloudinit.Render(goodParams())
	s := string(out)
	for _, want := range []string{"package_update: true", "package_upgrade: true"} {
		if !strings.Contains(s, want) {
			t.Errorf("user-data missing %q", want)
		}
	}
}

func TestRenderRebootsAtTheEnd(t *testing.T) {
	out, _ := cloudinit.Render(goodParams())
	s := string(out)
	if !strings.Contains(s, "power_state:") || !strings.Contains(s, "mode: reboot") {
		t.Errorf("user-data must end the instance with a reboot, got:\n%s", s)
	}
}

func TestRenderEmbedsInstallScriptVerbatim(t *testing.T) {
	p := goodParams()
	out, _ := cloudinit.Render(p)
	wantB64 := base64.StdEncoding.EncodeToString(p.InstallScript)
	if !strings.Contains(string(out), wantB64) {
		t.Fatalf("user-data does not embed the base64 of install.sh")
	}
	// And the base64 must round-trip back to the exact script bytes.
	decoded, err := base64.StdEncoding.DecodeString(wantB64)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(decoded) != string(p.InstallScript) {
		t.Fatalf("embedded script does not round-trip to the original")
	}
}

func TestRenderInjectsRepoTokenAndName(t *testing.T) {
	p := goodParams()
	out, _ := cloudinit.Render(p)
	env := extractEnv(t, string(out))
	for _, want := range []string{
		"RUNNER_URL='https://github.com/acme/example'",
		"RUNNER_TOKEN='TOKEN-A'",
		"RUNNER_NAME='ogrm-acme-001'",
	} {
		if !strings.Contains(env, want) {
			t.Errorf("env file missing line %q; env was:\n%s", want, env)
		}
	}
	if strings.Contains(env, "RUNNER_LABELS") {
		t.Errorf("env should omit RUNNER_LABELS when no labels are set, got:\n%s", env)
	}
}

func TestRenderIncludesLabelsWhenSet(t *testing.T) {
	p := goodParams()
	p.Labels = "self-hosted,linux,kind"
	out, _ := cloudinit.Render(p)
	env := extractEnv(t, string(out))
	if !strings.Contains(env, "RUNNER_LABELS='self-hosted,linux,kind'") {
		t.Errorf("env file missing RUNNER_LABELS line, got:\n%s", env)
	}
}

func TestRenderEscapesSingleQuotesInSecrets(t *testing.T) {
	p := goodParams()
	p.Token = "tok'en"
	out, _ := cloudinit.Render(p)
	env := extractEnv(t, string(out))
	// The classic '\'' idiom must appear so sourcing the file is safe.
	if !strings.Contains(env, `RUNNER_TOKEN='tok'\''en'`) {
		t.Errorf("single quote in token was not escaped, got:\n%s", env)
	}
}

func TestRenderPassesDiskGuardSettings(t *testing.T) {
	p := goodParams()
	p.DiskGuardThreshold = 70
	p.DiskGuardInterval = 20 * time.Minute
	out, err := cloudinit.Render(p)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	env := extractEnv(t, string(out))
	for _, want := range []string{
		"DISK_GUARD_ENABLED='true'",
		"DISK_GUARD_THRESHOLD='70'",
		// Seconds, not Go's "20m0s": the value lands in OnUnitActiveSec.
		"DISK_GUARD_INTERVAL='1200'",
	} {
		if !strings.Contains(env, want) {
			t.Errorf("env file missing line %q; env was:\n%s", want, env)
		}
	}
}

// A disabled guard must be stated explicitly, because install.sh defaults it
// to on: an omitted variable would silently install the guard anyway.
func TestRenderDisablesDiskGuardExplicitly(t *testing.T) {
	p := goodParams()
	p.DiskGuard = false
	out, err := cloudinit.Render(p)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	env := extractEnv(t, string(out))
	if !strings.Contains(env, "DISK_GUARD_ENABLED='false'") {
		t.Errorf("env file must switch the guard off explicitly, got:\n%s", env)
	}
	if strings.Contains(env, "DISK_GUARD_THRESHOLD") || strings.Contains(env, "DISK_GUARD_INTERVAL") {
		t.Errorf("a disabled guard needs no threshold or interval, got:\n%s", env)
	}
}

// The knobs are validated here rather than on the instance, where install.sh
// would reject them mid-boot with nobody watching.
func TestRenderRejectsImpossibleDiskGuardSettings(t *testing.T) {
	cases := map[string]func(*cloudinit.Params){
		"threshold zero":     func(p *cloudinit.Params) { p.DiskGuardThreshold = 0 },
		"threshold negative": func(p *cloudinit.Params) { p.DiskGuardThreshold = -1 },
		"threshold full":     func(p *cloudinit.Params) { p.DiskGuardThreshold = 100 },
		"interval too short": func(p *cloudinit.Params) { p.DiskGuardInterval = 30 * time.Second },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			p := goodParams()
			mutate(&p)
			if _, err := cloudinit.Render(p); err == nil {
				t.Fatalf("Render accepted params with %s", name)
			}
		})
	}
}

// With the guard off, its knobs are ignored rather than rejected — nothing on
// the instance reads them.
func TestRenderIgnoresDiskGuardSettingsWhenDisabled(t *testing.T) {
	p := goodParams()
	p.DiskGuard = false
	p.DiskGuardThreshold = 0
	p.DiskGuardInterval = 0
	if _, err := cloudinit.Render(p); err != nil {
		t.Fatalf("Render: %v", err)
	}
}

func TestRenderRequiresFields(t *testing.T) {
	cases := map[string]func(*cloudinit.Params){
		"no repo":   func(p *cloudinit.Params) { p.RepoURL = "" },
		"no token":  func(p *cloudinit.Params) { p.Token = "" },
		"no name":   func(p *cloudinit.Params) { p.RunnerName = "" },
		"no script": func(p *cloudinit.Params) { p.InstallScript = nil },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			p := goodParams()
			mutate(&p)
			if _, err := cloudinit.Render(p); err == nil {
				t.Fatalf("Render accepted params with %s", name)
			}
		})
	}
}

// extractEnv pulls the second write_files content block (the env file) out
// of the rendered document and base64-decodes it. The renderer writes the
// install script first and the env file second.
func extractEnv(t *testing.T, doc string) string {
	t.Helper()
	const marker = "content: "
	var blobs []string
	for _, line := range strings.Split(doc, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, marker) {
			blobs = append(blobs, strings.TrimPrefix(line, marker))
		}
	}
	if len(blobs) != 2 {
		t.Fatalf("expected exactly two base64 content blocks, found %d", len(blobs))
	}
	decoded, err := base64.StdEncoding.DecodeString(blobs[1])
	if err != nil {
		t.Fatalf("decode env block: %v", err)
	}
	return string(decoded)
}
