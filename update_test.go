package main

import "testing"

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		left, right string
		want        int
	}{
		{"v0.5.1", "v0.5.0", 1},
		{"v0.5.0", "v0.5.0", 0},
		{"v0.4.9", "v0.5.0", -1},
		{"v0.5.0", "v0.5.0-rc.1", 1},
		{"v0.5.0-rc.2", "v0.5.0-rc.1", 1},
	}
	for _, test := range tests {
		if got := compareVersions(test.left, test.right); got != test.want {
			t.Errorf("compareVersions(%q, %q)=%d, want %d", test.left, test.right, got, test.want)
		}
	}
}

func TestParseVersionRejectsUnversionedBuild(t *testing.T) {
	if _, ok := parseVersion("dev"); ok {
		t.Fatal("dev should not parse as a release version")
	}
}
