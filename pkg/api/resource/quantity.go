// Package resource implements Quantity, the fixed-point amounts used for
// resource requests and limits ("100m" CPU, "128Mi" memory), like
// k8s.io/apimachinery/pkg/api/resource in a much smaller form.
package resource

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
)

// Quantity is a non-negative amount kept in thousandths (milli-units), so
// "100m" CPU and "1Ki" bytes are both exact. It marshals back to the string it
// was parsed from.
type Quantity struct {
	milli int64
	s     string
}

// Suffixes and their multipliers. Decimal suffixes follow SI, binary ones
// IEC; "m" is one thousandth.
var suffixes = map[string]*big.Rat{
	"":   big.NewRat(1, 1),
	"n":  big.NewRat(1, 1e9),
	"u":  big.NewRat(1, 1e6),
	"m":  big.NewRat(1, 1000),
	"k":  big.NewRat(1e3, 1),
	"M":  big.NewRat(1e6, 1),
	"G":  big.NewRat(1e9, 1),
	"T":  big.NewRat(1e12, 1),
	"P":  big.NewRat(1e15, 1),
	"Ki": big.NewRat(1<<10, 1),
	"Mi": big.NewRat(1<<20, 1),
	"Gi": big.NewRat(1<<30, 1),
	"Ti": big.NewRat(1<<40, 1),
	"Pi": big.NewRat(1<<50, 1),
}

// ParseQuantity parses a decimal number with an optional suffix, such as
// "2", "0.5", "250m", "1.5Gi" or "500M". Fractions of a milli-unit round up.
func ParseQuantity(s string) (Quantity, error) {
	i := strings.IndexFunc(s, func(r rune) bool { return (r < '0' || r > '9') && r != '.' })
	number, suffix := s, ""
	if i >= 0 {
		number, suffix = s[:i], s[i:]
	}
	mult, ok := suffixes[suffix]
	if number == "" || strings.Count(number, ".") > 1 || number == "." || !ok {
		return Quantity{}, fmt.Errorf("quantities must match the regular expression '^([0-9.]+)([kMGTP]i?|[num])?$': %q", s)
	}
	value, ok := new(big.Rat).SetString(number)
	if !ok {
		return Quantity{}, fmt.Errorf("invalid quantity %q", s)
	}
	value.Mul(value, mult).Mul(value, big.NewRat(1000, 1))
	milli := new(big.Int).Quo(value.Num(), value.Denom())
	if !value.IsInt() {
		milli.Add(milli, big.NewInt(1))
	}
	if !milli.IsInt64() {
		return Quantity{}, fmt.Errorf("quantity %q is too large", s)
	}
	return Quantity{milli: milli.Int64(), s: s}, nil
}

// MustParse parses s and panics on error (for constants and tests).
func MustParse(s string) Quantity {
	q, err := ParseQuantity(s)
	if err != nil {
		panic(err)
	}
	return q
}

// NewMilliQuantity returns a quantity of milli thousandths, formatted like
// "500m", or "2" when whole.
func NewMilliQuantity(milli int64) Quantity {
	if milli%1000 == 0 {
		return Quantity{milli: milli, s: fmt.Sprint(milli / 1000)}
	}
	return Quantity{milli: milli, s: fmt.Sprintf("%dm", milli)}
}

// NewNanoQuantity returns a quantity of nano units, formatted like
// "117586291n" (how the metrics API reports CPU in cores).
func NewNanoQuantity(nano int64) Quantity {
	return Quantity{milli: (nano + 999_999) / 1_000_000, s: fmt.Sprintf("%dn", nano)}
}

// NewBinaryQuantity returns a byte count with the largest binary suffix
// that keeps it whole, like "2987404Ki" or "64Mi".
func NewBinaryQuantity(bytes int64) Quantity {
	for _, suffix := range []string{"Pi", "Ti", "Gi", "Mi", "Ki"} {
		unit := suffixes[suffix].Num().Int64()
		if bytes != 0 && bytes%unit == 0 {
			return Quantity{milli: bytes * 1000, s: fmt.Sprintf("%d%s", bytes/unit, suffix)}
		}
	}
	return NewQuantity(bytes)
}

// NewQuantity returns a whole quantity, such as a byte count.
func NewQuantity(value int64) Quantity { return Quantity{milli: value * 1000, s: fmt.Sprint(value)} }

// MilliValue returns the quantity in thousandths.
func (q Quantity) MilliValue() int64 { return q.milli }

// Value returns the quantity rounded up to a whole number.
func (q Quantity) Value() int64 { return (q.milli + 999) / 1000 }

// Cmp returns -1, 0 or 1 as q is less than, equal to or greater than o.
func (q Quantity) Cmp(o Quantity) int {
	switch {
	case q.milli < o.milli:
		return -1
	case q.milli > o.milli:
		return 1
	}
	return 0
}

// IsZero reports whether the quantity is zero.
func (q Quantity) IsZero() bool { return q.milli == 0 }

// String returns the quantity as written.
func (q Quantity) String() string {
	if q.s == "" {
		return "0"
	}
	return q.s
}

// MarshalJSON encodes the quantity as a string.
func (q Quantity) MarshalJSON() ([]byte, error) { return json.Marshal(q.String()) }

// UnmarshalJSON accepts a string ("500m") or a number (2, 0.5), as YAML
// manifests produce both.
func (q *Quantity) UnmarshalJSON(data []byte) error {
	text := string(bytes.TrimSpace(data))
	if strings.HasPrefix(text, `"`) {
		if err := json.Unmarshal(data, &text); err != nil {
			return err
		}
	}
	parsed, err := ParseQuantity(text)
	if err != nil {
		return err
	}
	*q = parsed
	return nil
}
