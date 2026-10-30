package main

import "testing"

func TestBuildVersionPrefersTheStampedVersion(t *testing.T) {
	was := version
	defer func() { version = was }()

	version = "8a439d2"
	if got := buildVersion(); got != "8a439d2" {
		t.Errorf("a stamped version must be reported as it is: %q", got)
	}
	version = "dev"
	if got := buildVersion(); got == "" {
		t.Error("with nothing stamped there must still be something to show (the embedded commit, or dev)")
	}
}
