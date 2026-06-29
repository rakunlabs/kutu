package upstream

import "testing"

// TestRouterFor verifies longest-prefix selection, leading-slash
// tolerance, default fallback, and that empty-prefix entries are
// ignored for routing.
func TestRouterFor(t *testing.T) {
	def, err := NewClient(Config{BaseURL: "https://default.example"})
	if err != nil {
		t.Fatalf("default client: %v", err)
	}
	acme, err := NewClient(Config{BaseURL: "https://acme.example"})
	if err != nil {
		t.Fatalf("acme client: %v", err)
	}
	sub, err := NewClient(Config{BaseURL: "https://sub.example"})
	if err != nil {
		t.Fatalf("sub client: %v", err)
	}

	r := NewRouter(def, []PrefixClient{
		{Prefix: "github.com/acme/", Client: acme},
		{Prefix: "github.com/acme/private/", Client: sub},
		{Prefix: "", Client: def}, // ignored: empty prefix
		{Prefix: "ignored.example/", Client: nil}, // ignored: nil client
	})

	cases := []struct {
		path string
		want *Client
	}{
		{"github.com/acme/foo", acme},
		{"github.com/acme/private/bar", sub},        // longest prefix wins
		{"/github.com/acme/private/bar", sub},       // leading slash trimmed
		{"github.com/other/x", def},                 // no match → default
		{"example.com/whatever", def},               // no match → default
	}
	for _, c := range cases {
		if got := r.For(c.path); got != c.want {
			gotURL, wantURL := "<nil>", "<nil>"
			if got != nil {
				gotURL = got.BaseURL()
			}
			if c.want != nil {
				wantURL = c.want.BaseURL()
			}
			t.Errorf("For(%q) = %s, want %s", c.path, gotURL, wantURL)
		}
	}

	if r.Default() != def {
		t.Errorf("Default() returned the wrong client")
	}
}
