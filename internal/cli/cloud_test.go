// SPDX-FileCopyrightText: Copyright 2026 B42Labs contributors
// SPDX-License-Identifier: BUSL-1.1

package cli

import "testing"

func TestChooseCloud(t *testing.T) {
	cases := []struct {
		name      string
		flag      string
		osCloud   string
		osAuthURL string
		want      string
	}{
		{"flag wins over everything", "devstack", "envcloud", "https://auth", "devstack"},
		{"OS_CLOUD set leaves it to clientconfig", "", "envcloud", "", ""},
		{"OS_AUTH_URL set leaves it to clientconfig", "", "", "https://auth", ""},
		{"nothing set falls back to openstack", "", "", "", "openstack"},
		{"blank flag is treated as unset", "   ", "", "", "openstack"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := chooseCloud(tc.flag, tc.osCloud, tc.osAuthURL); got != tc.want {
				t.Errorf("chooseCloud(%q, %q, %q) = %q; want %q", tc.flag, tc.osCloud, tc.osAuthURL, got, tc.want)
			}
		})
	}

	// The documented default must be the conventional single-entry name.
	if defaultCloudName != "openstack" {
		t.Errorf("defaultCloudName = %q; want %q", defaultCloudName, "openstack")
	}
}
