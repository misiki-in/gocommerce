package gocommerce

import "testing"

// TestDetectCarriers covers the line this file draws: a number with something
// distinctive in it identifies one carrier, a number that is only digits offers
// candidates, and a number that fits nobody offers nothing.
func TestDetectCarriers(t *testing.T) {
	cases := []struct {
		tracking string
		want     string // the first carrier, or "" for none
		only     bool   // true when it must be the only one offered
	}{
		// Distinctive: a prefix or suffix only one carrier uses.
		{"EE123456789IN", "india-post", true},
		{"ee123456789in", "india-post", true},    // case is the operator's, not the carrier's
		{"EE 1234 56789 IN", "india-post", true}, // as pasted off a label
		{"1Z999AA10123456784", "ups", true},
		{"JJD0099998888777766", "dhl", true},
		{"FMPP1234567890", "ekart", true},
		{"SF1234567890", "shadowfax", true},
		{"D12345678", "dtdc", true},

		// Ambiguous: several carriers issue numbers of this shape, so the
		// answer is a list and the first is a suggestion, not a verdict.
		{"12345678901", "delhivery", false},
		{"123456789012", "delhivery", false},

		// Nothing fits.
		{"", "", false},
		{"hello", "", false},
		{"12", "", false},
	}

	for _, c := range cases {
		got := DetectCarriers(c.tracking)
		if c.want == "" {
			if len(got) != 0 {
				t.Errorf("DetectCarriers(%q) = %v, want none", c.tracking, got)
			}
			continue
		}
		if len(got) == 0 {
			t.Errorf("DetectCarriers(%q) found nothing, want %s", c.tracking, c.want)
			continue
		}
		if got[0].Code != c.want {
			t.Errorf("DetectCarriers(%q) first = %s, want %s", c.tracking, got[0].Code, c.want)
		}
		if c.only && len(got) != 1 {
			t.Errorf("DetectCarriers(%q) = %d carriers, want only %s", c.tracking, len(got), c.want)
		}
		if !c.only && len(got) < 2 {
			t.Errorf("DetectCarriers(%q) offered one carrier for an ambiguous number", c.tracking)
		}
	}
}

// A tracking URL has to carry the number, and carry the cleaned-up one: the
// spaces somebody pasted are not part of it.
func TestCarrierTrackingURL(t *testing.T) {
	got, ok := DetectCarrier("1Z 999AA1 0123456784")
	if !ok {
		t.Fatal("a UPS number was not recognised")
	}
	want := "https://www.ups.com/track?tracknum=1Z999AA10123456784"
	if got.TrackURL != want {
		t.Errorf("TrackURL = %q, want %q", got.TrackURL, want)
	}

	// India Post has no URL that takes the number, and saying so is better than
	// linking somewhere that asks for it again.
	post, _ := DetectCarrier("EE123456789IN")
	if post.TrackURL != "" {
		t.Errorf("India Post TrackURL = %q, want none", post.TrackURL)
	}
}

func TestCarrierByCode(t *testing.T) {
	c, ok := CarrierByCode("delhivery", "12345678901")
	if !ok || c.Name != "Delhivery" {
		t.Fatalf("CarrierByCode(delhivery) = %+v, %v", c, ok)
	}
	if c.TrackURL == "" {
		t.Error("a stored carrier should still produce a link")
	}
	if _, ok := CarrierByCode("not-a-carrier", "123"); ok {
		t.Error("an unknown code was accepted")
	}
}

// TestCarriersRoster covers the picker's list rather than the matching: every
// carrier is offered, in a readable order, and the fallback is last.
func TestCarriersRoster(t *testing.T) {
	all := Carriers()
	if len(all) < 20 {
		t.Errorf("Carriers() = %d, want the full roster", len(all))
	}

	seen := map[string]bool{}
	for i, c := range all {
		if c.Code == "" || c.Name == "" {
			t.Errorf("carrier %d = %+v, want a code and a name", i, c)
		}
		if seen[c.Code] {
			t.Errorf("carrier %q appears twice", c.Code)
		}
		seen[c.Code] = true
		if c.TrackURL != "" {
			t.Errorf("%s came with a URL, but the roster has no number to put in one", c.Code)
		}
	}

	// The ones that matter to this store, and the ones a bare number can never
	// identify, are all offered.
	for _, want := range []string{"delhivery", "bluedart", "india-post", "dtdc",
		"ecom-express", "gati", "safexpress", "other"} {
		if !seen[want] {
			t.Errorf("%s is missing from the picker", want)
		}
	}

	// Alphabetical, so somebody looking for "Delhivery" can find it, with the
	// fallback pinned to the end where a fallback belongs.
	for i := 1; i < len(all)-1; i++ {
		if all[i-1].Name > all[i].Name {
			t.Errorf("%q sorts before %q", all[i-1].Name, all[i].Name)
			break
		}
	}
	if all[len(all)-1].Code != "other" {
		t.Errorf("last = %q, want the fallback", all[len(all)-1].Code)
	}
}

// A carrier with no pattern is nameable but never guessed: adding every plain-
// digit courier to the matching would answer "which carrier is this" with nine
// equally likely names, which is not an answer.
func TestPatternlessCarriersAreNeverGuessed(t *testing.T) {
	for _, tracking := range []string{"12345678901", "1234567890123", "123456789012"} {
		for _, c := range DetectCarriers(tracking) {
			switch c.Code {
			case "ecom-express", "gati", "safexpress", "trackon", "dpd", "tnt",
				"amazon-shipping", "professional-couriers", "other":
				t.Errorf("DetectCarriers(%q) offered %s, which has no pattern", tracking, c.Code)
			}
		}
	}
	// But a stored one still renders with its name and link.
	c, ok := CarrierByCode("ecom-express", "12345678901")
	if !ok || c.Name != "Ecom Express" || c.TrackURL == "" {
		t.Errorf("CarrierByCode(ecom-express) = %+v, %v", c, ok)
	}
}

// The S10 suffix is one rule that identifies a dozen national posts exactly.
func TestS10CountrySuffixes(t *testing.T) {
	cases := map[string]string{
		"EE123456789IN": "india-post",
		"LZ123456789US": "usps",
		"RB123456789GB": "royal-mail",
		"CP123456789AU": "australia-post",
		"LN123456789CA": "canada-post",
		"RA123456789CN": "china-post",
		"EJ123456789JP": "japan-post",
		"RR123456789SG": "singapore-post",
	}
	for tracking, want := range cases {
		got := DetectCarriers(tracking)
		if len(got) != 1 || got[0].Code != want {
			t.Errorf("DetectCarriers(%q) = %+v, want only %s", tracking, got, want)
		}
	}
	// A country nobody here handles is not claimed by the Indian one.
	if got := DetectCarriers("RR123456789DE"); len(got) != 0 {
		t.Errorf("DetectCarriers(a German S10) = %+v, want none", got)
	}
}
