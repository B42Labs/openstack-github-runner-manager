// SPDX-FileCopyrightText: Copyright 2026 B42Labs contributors
// SPDX-License-Identifier: BUSL-1.1

package openstack

import (
	"reflect"
	"testing"

	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/keypairs"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"

	"github.com/b42labs/openstack-github-runner-manager/internal/labels"
)

// An instance is created through keypairs.CreateOptsExt, which wraps the server
// options to add key_name. The wrapper rebuilds the request body, so this pins
// down that the tags still reach nova: losing them there would leave every
// instance unlabelled while every other resource stayed correct, and the failure
// would only surface as an empty `list -all`.
func TestServerCreateBodyCarriesTagsThroughTheKeypairWrapper(t *testing.T) {
	want := labels.ForIndex("ogrm", "acme", labels.RoleServer, 4).Tags()

	base := servers.CreateOpts{
		Name:      "ogrm-acme-004",
		FlavorRef: "flavor-id",
		Networks:  []servers.Network{{UUID: "net-id"}},
		Tags:      want,
	}
	withKey := keypairs.CreateOptsExt{CreateOptsBuilder: base, KeyName: "ogrm-acme-key"}

	body, err := withKey.ToServerCreateMap()
	if err != nil {
		t.Fatalf("build create body: %v", err)
	}

	server, ok := body["server"].(map[string]any)
	if !ok {
		t.Fatalf("create body has no server object: %v", body)
	}
	if server["key_name"] != "ogrm-acme-key" {
		t.Errorf("key_name = %v; want ogrm-acme-key", server["key_name"])
	}

	got, ok := server["tags"].([]any)
	if !ok {
		t.Fatalf("create body carries no tags: %v", server)
	}
	as := make([]string, len(got))
	for i, tag := range got {
		as[i], _ = tag.(string)
	}
	if !reflect.DeepEqual(as, want) {
		t.Errorf("tags = %v; want %v", as, want)
	}
}

// The shared infrastructure is unnumbered, so it must not pick up an index
// label. Nova and neutron would both accept ogrm:index=000, and it would then
// read as a real instance counter in the dashboard.
func TestUnnumberedResourceCarriesNoIndexTag(t *testing.T) {
	for _, tag := range labels.For("ogrm", "acme", labels.RoleNetwork).Tags() {
		if len(tag) >= len(labels.KeyIndex) && tag[:len(labels.KeyIndex)] == labels.KeyIndex {
			t.Errorf("network tags carry %q; the shared infrastructure has no index", tag)
		}
	}
}

// The repository an instance's runner registers against rides in the same
// create call, as server metadata. `delete` reads it back to deregister the
// runner, so losing it in the keypair wrapper would leave every runner
// registered after its instance is gone.
func TestServerCreateBodyCarriesTheRepoThroughTheKeypairWrapper(t *testing.T) {
	const repo = "https://github.com/acme/example"
	base := servers.CreateOpts{
		Name:      "ogrm-acme-004",
		FlavorRef: "flavor-id",
		Networks:  []servers.Network{{UUID: "net-id"}},
		Metadata:  repoMetadata(repo),
	}
	withKey := keypairs.CreateOptsExt{CreateOptsBuilder: base, KeyName: "ogrm-acme-key"}

	body, err := withKey.ToServerCreateMap()
	if err != nil {
		t.Fatalf("build create body: %v", err)
	}
	server, ok := body["server"].(map[string]any)
	if !ok {
		t.Fatalf("create body has no server object: %v", body)
	}
	md, ok := server["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("create body carries no metadata: %v", server)
	}
	if md[labels.KeyRepo] != repo {
		t.Errorf("metadata[%q] = %v; want %q", labels.KeyRepo, md[labels.KeyRepo], repo)
	}
}
