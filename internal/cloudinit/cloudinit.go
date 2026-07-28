// SPDX-FileCopyrightText: Copyright 2026 B42Labs contributors
// SPDX-License-Identifier: BUSL-1.1

// Package cloudinit renders the per-instance user-data that bootstraps a
// freshly booted Ubuntu VM into a self-hosted GitHub Actions runner.
//
// The rendered document is a #cloud-config that, in cloud-init's fixed
// module order:
//
//  1. updates and upgrades every apt package (package_update / package_upgrade),
//  2. writes the embedded install.sh and a root-only env file holding the
//     repository URL, the registration token, and the runner name,
//  3. runs install.sh with those values exported (runcmd), and
//  4. reboots the instance so the upgraded kernel and the freshly installed
//     runner service come up clean (power_state).
//
// The script and env file are base64-encoded into the document so neither
// the shell quoting in install.sh nor any token character can collide with
// YAML syntax.
package cloudinit

import (
	"encoding/base64"
	"fmt"
	"strings"
)

// scriptPath and envPath are where the document drops the installer and its
// environment on the instance. They live under /opt so they survive the
// post-install reboot for debugging.
const (
	scriptPath = "/opt/ogrm/install.sh"
	envPath    = "/opt/ogrm/runner.env"
)

// Params is the per-instance input to Render. RepoURL and InstallScript are
// identical across a fleet; Token and RunnerName differ per instance.
type Params struct {
	RepoURL       string
	Token         string
	RunnerName    string
	Labels        string // optional; empty falls back to install.sh's own default
	InstallScript []byte
}

// Render returns the user-data document for one instance. It returns an
// error only when a required field is missing, so callers can treat a
// successful return as a ready-to-submit blob.
func Render(p Params) ([]byte, error) {
	if p.RepoURL == "" {
		return nil, fmt.Errorf("cloudinit: repository URL is required")
	}
	if p.Token == "" {
		return nil, fmt.Errorf("cloudinit: token is required")
	}
	if p.RunnerName == "" {
		return nil, fmt.Errorf("cloudinit: runner name is required")
	}
	if len(p.InstallScript) == 0 {
		return nil, fmt.Errorf("cloudinit: install script is empty")
	}

	scriptB64 := base64.StdEncoding.EncodeToString(p.InstallScript)
	envB64 := base64.StdEncoding.EncodeToString([]byte(renderEnv(p)))

	var b strings.Builder
	b.WriteString("#cloud-config\n")
	// Refresh and upgrade every package before anything else runs. cloud-init
	// executes these in the config stage, strictly before runcmd, so the
	// installer always sees an up-to-date base image.
	b.WriteString("package_update: true\n")
	b.WriteString("package_upgrade: true\n")
	b.WriteString("write_files:\n")
	writeFile(&b, scriptPath, "0755", scriptB64)
	writeFile(&b, envPath, "0600", envB64)
	// Source the env file and run the installer. Splitting the env into its
	// own file keeps secrets out of the (process-listed) runcmd line and
	// dodges any shell-quoting hazard in the inline command.
	b.WriteString("runcmd:\n")
	b.WriteString(fmt.Sprintf("  - [ bash, -c, \"set -a && . %s && set +a && bash %s\" ]\n", envPath, scriptPath))
	// Reboot once the installer has registered the runner service so the
	// upgraded kernel is active and the systemd unit comes up on a clean boot.
	b.WriteString("power_state:\n")
	b.WriteString("  mode: reboot\n")
	b.WriteString("  condition: true\n")
	b.WriteString("  message: Rebooting after GitHub Actions runner provisioning\n")

	return []byte(b.String()), nil
}

// renderEnv builds the shell-sourced env file. Values are single-quoted and
// any embedded single quote is escaped, so a token or URL can carry shell
// metacharacters without breaking the `. runner.env` that sources it.
func renderEnv(p Params) string {
	var b strings.Builder
	b.WriteString("RUNNER_URL=" + shellSingleQuote(p.RepoURL) + "\n")
	b.WriteString("RUNNER_TOKEN=" + shellSingleQuote(p.Token) + "\n")
	b.WriteString("RUNNER_NAME=" + shellSingleQuote(p.RunnerName) + "\n")
	if p.Labels != "" {
		b.WriteString("RUNNER_LABELS=" + shellSingleQuote(p.Labels) + "\n")
	}
	return b.String()
}

// writeFile appends a single base64-encoded write_files entry.
func writeFile(b *strings.Builder, path, perms, contentB64 string) {
	fmt.Fprintf(b, "  - path: %s\n", path)
	b.WriteString("    owner: root:root\n")
	fmt.Fprintf(b, "    permissions: '%s'\n", perms)
	b.WriteString("    encoding: b64\n")
	fmt.Fprintf(b, "    content: %s\n", contentB64)
}

// shellSingleQuote wraps s in single quotes, escaping any single quote using
// the classic '\” idiom so the result is safe to source from /bin/sh.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
