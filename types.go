package gocommerce

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
)

// Money is how every amount crosses the API: an integer count of the
// currency's minor unit, plus the currency itself.
//
// It is never a float, because binary floating point cannot represent 0.10,
// and never a formatted string, because the number of decimal places is a
// property of the currency — USD has 2, JPY has 0, KWD has 3 — and the symbol
// and separators are properties of the reader's locale. Both belong to the
// client, which is precisely what lets a store change currency without the
// engine changing at all.
type Money struct {
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
}

func money(amountMinor int64, currency string) Money {
	return Money{AmountMinor: amountMinor, Currency: currency}
}

// Address is the shipping address as captured at checkout. It is stored as a
// snapshot on the order: structured, because a carrier module needs fields
// rather than a blob, and frozen, because an order's meaning must not change
// when someone edits their address later.
type Address struct {
	Name       string `json:"name,omitempty"`
	Phone      string `json:"phone,omitempty"`
	Line1      string `json:"line1"`
	Line2      string `json:"line2,omitempty"`
	City       string `json:"city"`
	State      string `json:"state,omitempty"`
	PostalCode string `json:"postal_code"`
	Country    string `json:"country"`
}

// Validate reports whether the address carries enough to ship to.
func (a Address) Validate() error {
	var missing []string
	if strings.TrimSpace(a.Line1) == "" {
		missing = append(missing, "line1")
	}
	if strings.TrimSpace(a.City) == "" {
		missing = append(missing, "city")
	}
	if strings.TrimSpace(a.PostalCode) == "" {
		missing = append(missing, "postal_code")
	}
	if strings.TrimSpace(a.Country) == "" {
		missing = append(missing, "country")
	}
	if len(missing) > 0 {
		return Validationf("address is missing %s", strings.Join(missing, ", "))
	}
	return nil
}

// Metadata is the extension point on core entities: a module attaches its own
// data under its own key rather than the engine growing a column for every
// integration that might ever exist.
type Metadata map[string]any

func (m Metadata) value() ([]byte, error) {
	if m == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(m)
}

func scanMetadata(raw []byte, dst *Metadata) error {
	if len(raw) == 0 {
		*dst = Metadata{}
		return nil
	}
	return json.Unmarshal(raw, dst)
}

// NullableInt64 is a patch field that can be set, cleared, or left alone.
//
// A plain *int64 collapses two different requests into one shape: a JSON body
// that omits the field and one that sends `null` both decode to nil, so a patch
// can set a reference but never remove it. That is fine until an operator picks
// a category and then wants "None" back — the panel sends null, the engine sees
// nothing to do, and the field silently refuses to clear.
//
// The three states are therefore made explicit. Present is what the patch acts
// on; Value nil within it means clear.
type NullableInt64 struct {
	Present bool
	Value   *int64
}

// NullableID is the same type where the value is a row id, which is most of the
// time. The alias exists so those call sites read as what they are.
type NullableID = NullableInt64

// NullableAmount is the same type where the value is money in minor units — a
// variant's cost, where an emptied box means "nobody has recorded one" and only
// an explicit null can say so.
type NullableAmount = NullableInt64

// UnmarshalJSON records that the field appeared at all, which is the whole
// point of the type — encoding/json only calls this for keys the body carries.
func (n *NullableInt64) UnmarshalJSON(b []byte) error {
	n.Present = true
	if string(b) == "null" {
		n.Value = nil
		return nil
	}
	var v int64
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	n.Value = &v
	return nil
}

// MarshalJSON round-trips the cleared and set forms. An absent field cannot be
// expressed by a value method, so it marshals as null; nothing in the engine
// sends a patch, and a test that builds one and encodes it should see the
// clearing form rather than a struct literal.
func (n NullableInt64) MarshalJSON() ([]byte, error) {
	if n.Value == nil {
		return []byte("null"), nil
	}
	return json.Marshal(*n.Value)
}

// SetID builds the "set it to this" form, for callers constructing a patch in
// Go rather than decoding one.
func SetID(id int64) NullableID { return NullableID{Present: true, Value: &id} }

// SetAmount and ClearAmount are the money-shaped constructors, so a cost patch
// does not have to be written as if it carried an id.
func SetAmount(minor int64) NullableAmount { return NullableAmount{Present: true, Value: &minor} }

// ClearAmount records "no amount", which for a cost is different from zero.
func ClearAmount() NullableAmount { return NullableAmount{Present: true} }

// ClearID builds the "remove the reference" form.
func ClearID() NullableID { return NullableID{Present: true} }

// token returns an unguessable opaque identifier. Cart tokens and order access
// tokens are the only credential a guest shopper ever holds, so they are
// 256 bits from crypto/rand and nothing else.
func token() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// slugify turns a title into a URL-safe slug. Callers may always supply their
// own; this is the convenience default.
func slugify(s string) string {
	var b strings.Builder
	lastDash := true // trims leading dashes
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			lastDash = false
		case !lastDash:
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}
