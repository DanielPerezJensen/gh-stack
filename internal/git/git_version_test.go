package git

import "testing"

func TestSupportsRebaseNoUpdateRefs(t *testing.T) {
	tests := []struct {
		version string
		want    bool
	}{
		{version: "git version 2.37.9", want: false},
		{version: "git version 2.38.0", want: true},
		{version: "git version 2.43.4", want: true},
		{version: "git version 3.0.0", want: true},
		{version: "invalid", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.version, func(t *testing.T) {
			if got := supportsRebaseNoUpdateRefs(tt.version); got != tt.want {
				t.Errorf("supportsRebaseNoUpdateRefs(%q) = %t, want %t", tt.version, got, tt.want)
			}
		})
	}
}
