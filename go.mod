// SPDX-FileCopyrightText: Copyright 2026 B42Labs contributors
// SPDX-License-Identifier: BUSL-1.1

// Module for ogrm (openstack-github-runner-manager), the tool that provisions
// and tears down fleets of self-hosted GitHub Actions runners on OpenStack.
// It talks to a live OpenStack cloud through the gophercloud SDK and ships
// with its own Makefile so it builds and tests in isolation.
module github.com/b42labs/openstack-github-runner-manager

go 1.26

require (
	github.com/gophercloud/gophercloud/v2 v2.14.0
	github.com/gophercloud/utils/v2 v2.0.0-20260424064311-2eeed4ceb3e9
)

require (
	github.com/gofrs/uuid/v5 v5.4.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.28.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)
