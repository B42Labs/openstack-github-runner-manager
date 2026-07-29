// SPDX-FileCopyrightText: Copyright 2026 B42Labs contributors
// SPDX-License-Identifier: BUSL-1.1

// Package cli is the command-line front end for the runner fleet tool. It
// parses arguments, prompts for the repository URL, mints the per-instance
// registration tokens through the GitHub CLI (or takes them from -token), and
// drives the openstack adapter to create, list, or delete a fleet.
package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
)

// Env carries the process-level dependencies a command needs, so Run is a
// pure function of its arguments and this struct — convenient for testing and
// for keeping main.go a three-liner.
type Env struct {
	// InstallScript is the embedded install.sh, rendered into each instance's
	// cloud-init user-data.
	InstallScript []byte

	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer

	Version string
	Commit  string
	Date    string
}

// Run dispatches a single command and returns a process exit code.
func Run(args []string, env *Env) int {
	if len(args) == 0 {
		usage(env.Stderr)
		return 2
	}

	cmd, rest := args[0], args[1:]
	var err error
	switch cmd {
	case "create", "up":
		err = runCreate(rest, env)
	case "delete", "destroy", "down":
		err = runDelete(rest, env)
	case "list", "ls":
		err = runList(rest, env)
	case "version", "--version", "-v":
		printVersion(env.Stdout, env)
		return 0
	case "help", "-h", "--help":
		usage(env.Stdout)
		return 0
	default:
		fmt.Fprintf(env.Stderr, "unknown command %q\n\n", cmd)
		usage(env.Stderr)
		return 2
	}

	if err != nil {
		// A -h on a subcommand surfaces as flag.ErrHelp; the FlagSet already
		// printed its usage, so exit cleanly.
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintf(env.Stderr, "error: %v\n", err)
		return 1
	}
	return 0
}

func printVersion(out io.Writer, env *Env) {
	fmt.Fprintf(out, "ogrm %s (commit %s, built %s)\n", env.Version, env.Commit, env.Date)
}

func usage(out io.Writer) {
	fmt.Fprint(out, `ogrm — provision self-hosted GitHub Actions runners on OpenStack

Usage:
  ogrm <command> [flags]

Commands:
  create    Create the network, router, keypair, volumes, and instances,
            then bootstrap each instance into a runner via cloud-init.
  list      Show the resources owned by a deployment, or every deployment
            in the project with -all.
  delete    Delete every resource owned by a deployment.
  version   Print version information.
  help      Show this help.

Run "ogrm <command> -h" for the flags of a command.

Authentication uses the ambient OpenStack credentials, exactly like the
openstack CLI: select a clouds.yaml entry with OS_CLOUD (or pass --cloud),
or export the OS_* environment variables. With none of those set, the
"openstack" clouds.yaml entry is used by default.
`)
}
