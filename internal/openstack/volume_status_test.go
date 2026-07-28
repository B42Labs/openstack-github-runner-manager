// SPDX-FileCopyrightText: Copyright 2026 B42Labs contributors
// SPDX-License-Identifier: BUSL-1.1

package openstack

import "testing"

func TestVolumeDeletable(t *testing.T) {
	deletable := []string{"available", "error", "error_restoring", "error_extending", "error_managing"}
	for _, status := range deletable {
		if !volumeDeletable(status) {
			t.Errorf("volumeDeletable(%q) = false; want true", status)
		}
	}

	// A still-attached or in-flight volume must NOT be considered deletable —
	// these are exactly the states Cinder rejects a delete for, so teardown
	// must keep waiting.
	notYet := []string{"in-use", "attaching", "detaching", "reserved", "creating", "deleting", "maintenance", "backing-up"}
	for _, status := range notYet {
		if volumeDeletable(status) {
			t.Errorf("volumeDeletable(%q) = true; want false (must wait)", status)
		}
	}
}
