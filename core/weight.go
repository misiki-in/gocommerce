package gocommerce

import (
	"fmt"
	"math"
	"strings"
)

// Weight units.
//
// The rule is money's rule: one canonical stored value plus the unit it is
// read in. `weight_grams` is the fact — integer, exact, what a carrier API is
// given — and `weight_unit` is presentation, exactly as a currency code is.
//
// Conversion lives here and nowhere else. The moment two files each know that
// a pound is 453.59237 grams, they will eventually disagree by a rounding rule
// and a package will be quoted at the wrong price.

// The units a weight may be entered in.
const (
	WeightGram     = "g"
	WeightKilogram = "kg"
	WeightOunce    = "oz"
	WeightPound    = "lb"
)

// DefaultWeightUnit is what a variant gets when nothing says otherwise. Grams,
// because it is the canonical unit and needs no conversion to be correct.
const DefaultWeightUnit = WeightGram

// gramsPer holds the exact international definitions. The avoirdupois pound is
// 453.59237 g by definition, not by measurement, so these are not
// approximations to be tidied up later.
var gramsPer = map[string]float64{
	WeightGram:     1,
	WeightKilogram: 1000,
	WeightOunce:    28.349523125,
	WeightPound:    453.59237,
}

// ValidWeightUnit reports whether unit is one the engine stores.
func ValidWeightUnit(unit string) bool {
	_, ok := gramsPer[unit]
	return ok
}

// NormalizeWeightUnit accepts what a person or an import file is likely to
// write and returns the stored form. An unrecognised unit is an error rather
// than a silent fallback to grams: quietly reading "2.5 lbs" as 2.5 grams
// would under-quote shipping by a factor of 180.
func NormalizeWeightUnit(unit string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(unit)) {
	case "", "g", "gram", "grams", "gramme", "grammes":
		return WeightGram, nil
	case "kg", "kilo", "kilos", "kilogram", "kilograms", "kilogramme", "kilogrammes":
		return WeightKilogram, nil
	case "oz", "ounce", "ounces":
		return WeightOunce, nil
	case "lb", "lbs", "pound", "pounds":
		return WeightPound, nil
	}
	return "", Validationf("%q is not a weight unit; use g, kg, oz or lb", unit)
}

// GramsFrom converts a quantity in unit into whole grams.
//
// It rounds to the nearest gram, which is the resolution the column stores.
// A quarter-ounce is 7.09 g and becomes 7 — losing 0.09 g matters to nobody,
// whereas storing a float that cannot represent 0.1 exactly would eventually
// produce a total that does not add up.
func GramsFrom(value float64, unit string) (int, error) {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, Validationf("weight must be a number")
	}
	if value < 0 {
		return 0, Validationf("weight cannot be negative")
	}
	per, ok := gramsPer[unit]
	if !ok {
		return 0, Validationf("%q is not a weight unit; use g, kg, oz or lb", unit)
	}
	grams := math.Round(value * per)
	// An int overflow here would silently wrap into a negative weight, which
	// the CHECK constraint would then reject with a baffling message.
	if grams > math.MaxInt32 {
		return 0, Validationf("that weight is implausible")
	}
	return int(grams), nil
}

// InUnit converts stored grams into the given unit for display.
//
// The result is a float because that is what a person reads: 2.5 kg, not 2500.
// It is never what gets stored — the grams are.
func InUnit(grams int, unit string) float64 {
	per, ok := gramsPer[unit]
	if !ok || per == 0 {
		return float64(grams)
	}
	return float64(grams) / per
}

// resolveWeight turns whatever a caller supplied into the pair the column
// stores: whole grams, and the unit to read them in.
//
// A client may send grams directly (the API's shape) or a value plus a unit
// (a form's shape). When both arrive, the value wins — a caller that computed
// grams itself and then also sent "2.5 kg" has contradicted itself, and the
// human-entered number is the one a person can check.
func resolveWeight(grams *int, value *float64, unit string) (*int, string, error) {
	normalized, err := NormalizeWeightUnit(unit)
	if err != nil {
		return nil, "", err
	}

	switch {
	case value != nil:
		g, err := GramsFrom(*value, normalized)
		if err != nil {
			return nil, "", err
		}
		return &g, normalized, nil
	case grams != nil:
		if *grams < 0 {
			return nil, "", Validationf("weight cannot be negative")
		}
		return grams, normalized, nil
	}
	// No weight at all. The unit still travels, so a variant that gets one
	// later is read in the unit the operator already chose.
	return nil, normalized, nil
}

// FormatWeight renders a stored weight the way its unit wants to be read.
//
// Grams get no decimals because a fractional gram is below the stored
// resolution; the others get up to three, with trailing zeros trimmed, so
// 2.5 kg reads as "2.5 kg" rather than "2.500 kg".
func FormatWeight(grams int, unit string) string {
	if unit == WeightGram || !ValidWeightUnit(unit) {
		return fmt.Sprintf("%d g", grams)
	}
	s := strings.TrimRight(strings.TrimRight(
		fmt.Sprintf("%.3f", InUnit(grams, unit)), "0"), ".")
	if s == "" || s == "-" {
		s = "0"
	}
	return s + " " + unit
}
