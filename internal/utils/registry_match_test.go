package utils

import "testing"

// TestRegistryMatches guards against credential leakage from the Helm registry
// secret matcher. See the security fix for issue described in the PR: the
// matcher must anchor on a path boundary and reject an empty repository, so a
// secret's credentials are only offered to the registry they were issued for.
func TestRegistryMatches(t *testing.T) {
	cases := []struct {
		name string
		reg  string
		repo string
		want bool
	}{
		// Legitimate matches.
		{"exact host", "myregistry.io", "myregistry.io", true},
		{"host with chart path", "myregistry.io/charts/foo", "myregistry.io", true},
		{"host+path prefix", "myregistry.io/charts/foo", "myregistry.io/charts", true},
		{"host with port and path", "myregistry.io:5000/charts/foo", "myregistry.io:5000", true},

		// Credential-leak vectors that must NOT match.
		{"lookalike host suffix", "myregistry.io.attacker.com/foo", "myregistry.io", false},
		{"empty repository matches nothing", "myregistry.io/charts/foo", "", false},
		{"partial path segment", "myregistry.io/chartsfoo", "myregistry.io/charts", false},
		{"different host", "other.io/foo", "myregistry.io", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := registryMatches(tc.reg, tc.repo); got != tc.want {
				t.Fatalf("registryMatches(%q, %q) = %v, want %v", tc.reg, tc.repo, got, tc.want)
			}
		})
	}
}
