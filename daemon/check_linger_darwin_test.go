//go:build darwin

package daemon

import "testing"

func TestCheckLinger_DarwinAlwaysEnabled(t *testing.T) {
	enabled, user := CheckLinger()
	if !enabled || user != "" {
		t.Fatalf("CheckLinger() = (%v, %q), want (true, empty)", enabled, user)
	}
}
