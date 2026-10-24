// Package quantity parses resource amounts written in Kubernetes Quantity
// syntax ("2", "500m", "512Mi", "1.5e3") exactly, and formats the canonical
// forms Hangar stores. It is not a full port of the Kubernetes type: there is
// no arithmetic on quantities, and the n and u suffixes are not accepted.
package quantity

import (
	"fmt"
	"math"
	"math/big"
	"strconv"
)

// Byte multiples used by the canonical forms.
const (
	MiB uint64 = 1 << 20
	GiB uint64 = 1 << 30
)

// maxExponent bounds a decimal exponent, so that parsing "1e999999999" cannot
// build an enormous number. Anything this large overflows every typed helper
// anyway.
const maxExponent = 100

var suffixes = map[string]*big.Rat{
	"Ki": pow(2, 10), "Mi": pow(2, 20), "Gi": pow(2, 30),
	"Ti": pow(2, 40), "Pi": pow(2, 50), "Ei": pow(2, 60),
	"k": pow(10, 3), "M": pow(10, 6), "G": pow(10, 9),
	"T": pow(10, 12), "P": pow(10, 15), "E": pow(10, 18),
	"m": new(big.Rat).SetFrac64(1, 1000),
}

func pow(base, exp int64) *big.Rat {
	return new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(base), big.NewInt(exp), nil))
}

// Parse returns the exact value of s.
func Parse(s string) (*big.Rat, error) {
	num, suffix := split(s)
	// num holds only a sign, digits and dots, so SetString reads it as a
	// decimal and refuses anything but one number.
	v, ok := new(big.Rat).SetString(num)
	if !ok {
		return nil, fmt.Errorf("%q is not a quantity, such as 2, 500m, 512Mi or 10Gi", s)
	}
	if suffix == "" {
		return v, nil
	}
	if mult, ok := suffixes[suffix]; ok {
		return v.Mul(v, mult), nil
	}
	if (suffix[0] == 'e' || suffix[0] == 'E') && validExponent(suffix[1:]) {
		exp, err := strconv.Atoi(suffix[1:])
		if err != nil || exp > maxExponent || exp < -maxExponent {
			return nil, fmt.Errorf("%q: the exponent is out of range", s)
		}
		if exp >= 0 {
			return v.Mul(v, pow(10, int64(exp))), nil
		}
		return v.Quo(v, pow(10, int64(-exp))), nil
	}
	return nil, fmt.Errorf("%q has an unknown suffix %q; use Ki, Mi, Gi, Ti, Pi, Ei, k, M, G, T, P, E, m or an exponent", s, suffix)
}

// split separates the signed decimal number at the start of s from what
// follows it.
func split(s string) (num, suffix string) {
	i := 0
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		i++
	}
	for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.') {
		i++
	}
	return s[:i], s[i:]
}

// validExponent accepts an optional sign and at least one digit.
func validExponent(e string) bool {
	if e != "" && (e[0] == '+' || e[0] == '-') {
		e = e[1:]
	}
	if e == "" {
		return false
	}
	for _, c := range e {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// CPU parses a vCPU count: a whole number of at least one.
func CPU(s string) (int32, error) {
	v, err := Parse(s)
	if err != nil {
		return 0, err
	}
	if !v.IsInt() {
		return 0, fmt.Errorf("cpu %q is a fraction; a VM's vCPUs are whole, such as 1 or 2", s)
	}
	n := v.Num()
	if n.Sign() < 1 {
		return 0, fmt.Errorf("cpu %q must be at least 1", s)
	}
	if !n.IsInt64() || n.Int64() > math.MaxInt32 {
		return 0, fmt.Errorf("cpu %q is too large", s)
	}
	return int32(n.Int64()), nil
}

// Bytes parses a size in bytes: whole, not negative, and within uint64.
func Bytes(s string) (uint64, error) {
	v, err := Parse(s)
	if err != nil {
		return 0, err
	}
	if v.Sign() < 0 {
		return 0, fmt.Errorf("%q is negative", s)
	}
	if !v.IsInt() {
		return 0, fmt.Errorf("%q is not a whole number of bytes", s)
	}
	if !v.Num().IsUint64() {
		return 0, fmt.Errorf("%q is too large", s)
	}
	return v.Num().Uint64(), nil
}

// FormatCPU is the canonical form of a vCPU count.
func FormatCPU(n int32) string {
	return strconv.FormatInt(int64(n), 10)
}

// FormatMemory is the canonical form of a memory size: whole Mi, rounded up.
func FormatMemory(bytes uint64) string {
	return strconv.FormatUint(ceilDiv(bytes, MiB), 10) + "Mi"
}

// FormatDisk is the canonical form of a disk size: whole Gi, rounded up.
func FormatDisk(bytes uint64) string {
	return strconv.FormatUint(ceilDiv(bytes, GiB), 10) + "Gi"
}

// ceilDiv is a / b rounded up, without the overflow of (a + b - 1) / b.
func ceilDiv(a, b uint64) uint64 {
	q := a / b
	if a%b != 0 {
		q++
	}
	return q
}
