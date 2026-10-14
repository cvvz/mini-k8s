package intstr

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// GetScaledValue resolves a number or a percentage ("25%") of total,
// rounding up or down, like Kubernetes' GetScaledValueFromIntOrPercent.
func GetScaledValue(v *IntOrString, total int, roundUp bool) (int, error) {
	if v == nil {
		return 0, nil
	}
	if !v.IsString {
		return int(v.IntVal), nil
	}
	pct, ok := strings.CutSuffix(v.StrVal, "%")
	if !ok {
		return 0, fmt.Errorf("invalid value %q: must be an integer or a percentage", v.StrVal)
	}
	n, err := strconv.Atoi(pct)
	if err != nil {
		return 0, fmt.Errorf("invalid value %q: must be an integer or a percentage", v.StrVal)
	}
	scaled := float64(n) * float64(total) / 100
	if roundUp {
		return int(math.Ceil(scaled)), nil
	}
	return int(math.Floor(scaled)), nil
}
