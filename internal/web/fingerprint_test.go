package web

import "testing"

func TestNormalizeFingerprint(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"déjà propre 16 chars", "a4f4d1ec8ff1376e", "a4f4d1ec8ff1376e"},
		{"32 chars → tronqué", "a4f4d1ec8ff1376e452d42764be68ecc", "a4f4d1ec8ff1376e"},
		{"avec tirets UUID-like", "a4f4-d1ec-8ff1-376e", "a4f4d1ec8ff1376e"},
		{"32 chars + tirets", "a4f4-d1ec-8ff1-376e-452d-4276-4be6-8ecc", "a4f4d1ec8ff1376e"},
		{"majuscules", "A4F4D1EC8FF1376E", "a4f4d1ec8ff1376e"},
		{"espaces", "  a4f4d1ec8ff1376e  ", "a4f4d1ec8ff1376e"},
		{"espaces internes", "a4f4 d1ec 8ff1 376e", "a4f4d1ec8ff1376e"},
		{"vide", "", ""},
		{"déjà tronqué moins de 16", "a4f4d1ec", "a4f4d1ec"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := normalizeFingerprint(tc.in)
			if got != tc.want {
				t.Errorf("normalizeFingerprint(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
