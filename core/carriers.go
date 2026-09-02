package gocommerce

import (
	"regexp"
	"sort"
	"strings"
)

// Working out which carrier a tracking number belongs to.
//
// The honest position first: this cannot always be done. A tracking number is
// not a self-describing identifier — several carriers issue bare ten- to
// fourteen-digit numbers with no prefix and no checksum anyone publishes, so a
// twelve-digit number is genuinely ambiguous and no amount of pattern matching
// makes it otherwise.
//
// So the answer is a *list*, best first, rather than a verdict. Where a number
// carries something distinctive — UPS's 1Z, the S10 standard's country suffix,
// DHL's JJD, Ekart's FMPP — the list has one entry and the panel fills it in.
// Where it does not, the operator picks from the carriers whose format fits.
// The alternative is guessing, and a wrong guess is worse than no guess: it
// hands the customer a tracking link that goes nowhere, which reads as the shop
// having lost their parcel.

// Carrier is a shipping company a tracking number might belong to.
type Carrier struct {
	Code string `json:"code"`
	Name string `json:"name"`
	// TrackURL is where a customer follows the parcel, with the tracking
	// number already in it. Empty when the carrier has no URL that takes the
	// number as a parameter — India Post's form posts it — and a client should
	// then show the number without a link rather than a link that fails.
	TrackURL string `json:"track_url,omitempty"`
}

// carrierPattern is one carrier and, where there is one, the shape of the
// numbers it issues.
type carrierPattern struct {
	code, name, urlTemplate string
	// match is nil for a carrier that can be recorded but never guessed. Most
	// couriers issue plain digits, and adding every one of them to the matching
	// would turn a useful suggestion into a list of nine equally likely
	// answers. They belong in the picker all the same: an operator who knows
	// who has the parcel should be able to say so.
	match *regexp.Regexp
	// distinctive marks a pattern that only one carrier uses — a prefix or a
	// suffix rather than a length. A distinctive match is reported on its own
	// even when looser patterns also fit, because it is an identification
	// rather than a candidate.
	distinctive bool
}

// The Indian carriers come first because that is where this engine is used, and
// within each group the distinctive patterns come before the length-only ones.
// Order decides what a client pre-selects.
var carrierPatterns = []carrierPattern{
	// S10 (UPU standard): two letters, nine digits, the origin country. Every
	// national post uses it, so the country code is what names the carrier —
	// one rule that identifies a dozen of them, and identifies them exactly.
	{
		code: "india-post", name: "India Post",
		match:       regexp.MustCompile(`^[A-Z]{2}[0-9]{9}IN$`),
		distinctive: true,
		// The tracking page posts its form, so there is no URL that carries the
		// number. Better to say so than to link somewhere that asks again.
	},
	{
		code: "usps", name: "USPS",
		match:       regexp.MustCompile(`^[A-Z]{2}[0-9]{9}US$`),
		distinctive: true,
		urlTemplate: "https://tools.usps.com/go/TrackConfirmAction?tLabels=%s",
	},
	{
		code: "royal-mail", name: "Royal Mail",
		match:       regexp.MustCompile(`^[A-Z]{2}[0-9]{9}GB$`),
		distinctive: true,
		urlTemplate: "https://www.royalmail.com/track-your-item#/tracking-results/%s",
	},
	{
		code: "australia-post", name: "Australia Post",
		match:       regexp.MustCompile(`^[A-Z]{2}[0-9]{9}AU$`),
		distinctive: true,
		urlTemplate: "https://auspost.com.au/mypost/track/#/details/%s",
	},
	{
		code: "canada-post", name: "Canada Post",
		match:       regexp.MustCompile(`^[A-Z]{2}[0-9]{9}CA$`),
		distinctive: true,
	},
	{
		code: "china-post", name: "China Post",
		match:       regexp.MustCompile(`^[A-Z]{2}[0-9]{9}CN$`),
		distinctive: true,
	},
	{
		code: "japan-post", name: "Japan Post",
		match:       regexp.MustCompile(`^[A-Z]{2}[0-9]{9}JP$`),
		distinctive: true,
	},
	{
		code: "singapore-post", name: "Singapore Post",
		match:       regexp.MustCompile(`^[A-Z]{2}[0-9]{9}SG$`),
		distinctive: true,
	},
	{
		code: "ekart", name: "Ekart",
		match:       regexp.MustCompile(`^FM(PP|PC|SD)[0-9]{10,}$`),
		distinctive: true,
		urlTemplate: "https://ekartlogistics.com/shipmenttrack/%s",
	},
	{
		code: "shadowfax", name: "Shadowfax",
		match:       regexp.MustCompile(`^SF[0-9]{9,}$`),
		distinctive: true,
		urlTemplate: "https://tracker.shadowfax.in/#/tracking/%s",
	},
	{
		code: "dtdc", name: "DTDC",
		match:       regexp.MustCompile(`^[A-Z][0-9]{8}$`),
		distinctive: true,
		urlTemplate: "https://www.dtdc.in/tracking/tracking_results.asp?strCnno=%s",
	},
	{
		code: "ups", name: "UPS",
		match:       regexp.MustCompile(`^1Z[0-9A-Z]{16}$`),
		distinctive: true,
		urlTemplate: "https://www.ups.com/track?tracknum=%s",
	},
	{
		code: "dhl", name: "DHL",
		match:       regexp.MustCompile(`^(JJD|JVGL|GM|LX|RX)[0-9A-Z]{8,}$`),
		distinctive: true,
		urlTemplate: "https://www.dhl.com/in-en/home/tracking.html?tracking-id=%s",
	},

	// From here the patterns are lengths, and lengths overlap. Everything below
	// is a candidate, never an identification.
	{
		code: "delhivery", name: "Delhivery",
		match:       regexp.MustCompile(`^[0-9]{11,14}$`),
		urlTemplate: "https://www.delhivery.com/track/package/%s",
	},
	{
		code: "bluedart", name: "Blue Dart",
		match:       regexp.MustCompile(`^[0-9]{11,12}$`),
		urlTemplate: "https://www.bluedart.com/web/guest/trackdartresult?trackFor=0&trackNo=%s",
	},
	{
		code: "xpressbees", name: "XpressBees",
		match:       regexp.MustCompile(`^[0-9]{13,14}$`),
		urlTemplate: "https://www.xpressbees.com/shipment/tracking?awbNo=%s",
	},
	{
		code: "fedex", name: "FedEx",
		match:       regexp.MustCompile(`^([0-9]{12}|[0-9]{15}|[0-9]{20}|[0-9]{22})$`),
		urlTemplate: "https://www.fedex.com/fedextrack/?trknbr=%s",
	},
	{
		code: "aramex", name: "Aramex",
		match:       regexp.MustCompile(`^[0-9]{10,11}$`),
		urlTemplate: "https://www.aramex.com/track/results?ShipmentNumber=%s",
	},

	// Nameable, never guessed. These issue plain digits like the five above,
	// and matching them too would answer "which carrier is this" with nine
	// names — which is not an answer. They are here so an operator who knows
	// can record it, and so a stored code renders with its name and link.
	{
		code: "ecom-express", name: "Ecom Express",
		urlTemplate: "https://ecomexpress.in/tracking/?awb_field=%s",
	},
	{code: "amazon-shipping", name: "Amazon Shipping"},
	{code: "professional-couriers", name: "The Professional Couriers"},
	{code: "gati", name: "Gati"},
	{code: "safexpress", name: "Safexpress"},
	{code: "trackon", name: "Trackon"},
	{code: "dpd", name: "DPD"},
	{
		code: "tnt", name: "TNT",
		urlTemplate: "https://www.tnt.com/express/en_in/site/tracking.html?searchType=con&cons=%s",
	},
	// Last, and deliberately: a store that hands parcels to somebody with no
	// entry here can still say the number belongs to a real courier rather than
	// leaving the field blank and losing the fact.
	{code: "other", name: "Another carrier"},
}

// normalizeTracking strips what people paste around a tracking number: spaces,
// hyphens the carrier prints for legibility, and the case they happened to type.
func normalizeTracking(tracking string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(strings.TrimSpace(tracking)) {
		switch {
		case r >= '0' && r <= '9', r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		}
	}
	return b.String()
}

// DetectCarriers returns the carriers whose numbering a tracking number fits,
// best first. It is empty when nothing fits, and has one entry when the number
// carries something only one carrier uses.
//
// A caller that wants a single answer should take the first and let somebody
// correct it — see the comment at the top of this file for why there is not
// always a right one.
func DetectCarriers(tracking string) []Carrier {
	n := normalizeTracking(tracking)
	if n == "" {
		return nil
	}

	var all []Carrier
	for _, p := range carrierPatterns {
		if p.match == nil || !p.match.MatchString(n) {
			continue
		}
		c := Carrier{Code: p.code, Name: p.name}
		if p.urlTemplate != "" {
			c.TrackURL = strings.Replace(p.urlTemplate, "%s", n, 1)
		}
		if p.distinctive {
			// Nothing else is worth offering beside it.
			return []Carrier{c}
		}
		all = append(all, c)
	}
	return all
}

// DetectCarrier returns the single best guess, and whether there was one.
func DetectCarrier(tracking string) (Carrier, bool) {
	found := DetectCarriers(tracking)
	if len(found) == 0 {
		return Carrier{}, false
	}
	return found[0], true
}

// CarrierByCode looks up a carrier the operator has already chosen, so a stored
// code can be rendered with its name and a link.
func CarrierByCode(code, tracking string) (Carrier, bool) {
	for _, p := range carrierPatterns {
		if p.code != code {
			continue
		}
		c := Carrier{Code: p.code, Name: p.name}
		if p.urlTemplate != "" && tracking != "" {
			c.TrackURL = strings.Replace(p.urlTemplate, "%s", normalizeTracking(tracking), 1)
		}
		return c, true
	}
	return Carrier{}, false
}

// Carriers lists every carrier this engine can name, for a picker.
//
// Alphabetical, because a picker is read rather than reasoned about — the order
// the patterns are declared in serves matching, not somebody looking for
// "Delhivery". "Another carrier" sits at the end, where a fallback belongs.
func Carriers() []Carrier {
	out := make([]Carrier, 0, len(carrierPatterns))
	for _, p := range carrierPatterns {
		out = append(out, Carrier{Code: p.code, Name: p.name})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if (out[i].Code == "other") != (out[j].Code == "other") {
			return out[j].Code == "other"
		}
		return out[i].Name < out[j].Name
	})
	return out
}
