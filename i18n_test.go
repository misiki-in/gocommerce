package gocommerce

import (
	"reflect"
	"testing"
)

func TestNegotiateLanguage(t *testing.T) {
	t.Parallel()

	available := []string{"en", "fr", "de-CH"}

	tests := []struct {
		name   string
		avail  []string
		query  string
		accept string
		want   string
	}{
		{name: "no preference falls back to default", avail: available, want: "en"},
		{name: "explicit query wins", avail: available, query: "fr", accept: "de", want: "fr"},
		{name: "query is matched case-insensitively", avail: available, query: "FR", want: "fr"},
		{
			name:  "unsupported query falls back to default, not to the header",
			avail: available, query: "es", accept: "fr", want: "en",
		},
		{name: "header exact match", avail: available, accept: "fr", want: "fr"},
		{
			name:  "header primary-subtag match serves the base language",
			avail: available, accept: "en-GB", want: "en",
		},
		{
			name:  "quality values order the candidates",
			avail: available, accept: "es;q=0.9,fr;q=1.0", want: "fr",
		},
		{
			name:  "an unsupported first choice yields to the next",
			avail: available, accept: "es,fr;q=0.5", want: "fr",
		},
		{
			name:  "available tag with a region matches a bare request",
			avail: available, accept: "de", want: "de-CH",
		},
		{name: "wildcard means the default", avail: available, accept: "*", want: "en"},
		{name: "q=0 entries are ignored", avail: available, accept: "fr;q=0", want: "en"},
		{name: "malformed header does not fail the request", avail: available, accept: ",,;q=,", want: "en"},
		{
			name:  "unsupported header falls back to default",
			avail: available, accept: "ja,ko;q=0.8", want: "en",
		},
		{name: "empty available set yields the default", avail: nil, accept: "fr", want: "en"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := negotiateLanguage(tc.avail, "en", tc.query, tc.accept)
			if got != tc.want {
				t.Errorf("negotiateLanguage(%v, \"en\", %q, %q) = %q, want %q",
					tc.avail, tc.query, tc.accept, got, tc.want)
			}
		})
	}
}

func TestParseAcceptLanguage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		header string
		want   []string
	}{
		{"", nil},
		{"en", []string{"en"}},
		{"en-US,en;q=0.9,fr;q=0.8", []string{"en-US", "en", "fr"}},
		{"fr;q=0.2,de;q=0.9", []string{"de", "fr"}},
		// Equal quality preserves the header's own order.
		{"a;q=0.5,b;q=0.5", []string{"a", "b"}},
	}

	for _, tc := range tests {
		got := parseAcceptLanguage(tc.header)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("parseAcceptLanguage(%q) = %v, want %v", tc.header, got, tc.want)
		}
	}
}

func TestConfigLanguageDefaults(t *testing.T) {
	t.Parallel()

	t.Run("defaults to en", func(t *testing.T) {
		cfg := Config{DBURL: "x", Dev: true}
		if err := cfg.applyDefaults(); err != nil {
			t.Fatal(err)
		}
		if cfg.DefaultLanguage != "en" {
			t.Errorf("DefaultLanguage = %q, want en", cfg.DefaultLanguage)
		}
		if !reflect.DeepEqual(cfg.Languages, []string{"en"}) {
			t.Errorf("Languages = %v, want [en]", cfg.Languages)
		}
		if cfg.Currency != "USD" {
			t.Errorf("Currency = %q, want USD", cfg.Currency)
		}
	})

	t.Run("default language is always served", func(t *testing.T) {
		cfg := Config{DBURL: "x", Dev: true, DefaultLanguage: "fr", Languages: []string{"en", "de"}}
		if err := cfg.applyDefaults(); err != nil {
			t.Fatal(err)
		}
		if !containsFold(cfg.Languages, "fr") {
			t.Errorf("Languages = %v, want it to contain the default language fr", cfg.Languages)
		}
	})
}
