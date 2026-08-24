// SPDX-License-Identifier: AGPL-3.0-or-later

package controlplane

import (
	"strings"
	"testing"
)

func TestNameValidation(t *testing.T) {
	valid := []string{"qa", "agent-bob", "feature-123", "a", "x1-y2-z3"}
	invalid := []string{
		"",                       // empty
		"-bad",                   // leading dash
		"Bad",                    // uppercase
		"a b",                    // space
		"under_score",            // underscore
		strings.Repeat("a", 50),  // too long (max 41)
	}
	for _, n := range valid {
		if !nameRe.MatchString(n) {
			t.Errorf("expected %q to be valid", n)
		}
	}
	for _, n := range invalid {
		if nameRe.MatchString(n) {
			t.Errorf("expected %q to be invalid", n)
		}
	}
}
