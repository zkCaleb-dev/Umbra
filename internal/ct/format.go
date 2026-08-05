package ct

import (
	"math/big"
	"strings"
)

// StellarAssetDecimals is the precision of a Stellar Asset Contract: the
// built-in SAC always reports 7, so every classic-asset-backed
// confidential token uses it. Confidential-token amounts travel as
// integer base units (stroops); this is the divisor that turns them
// into human units for display. A CT wrapping a non-SAC SEP-41 token
// with different precision would need this queried per-asset — expose
// the value through the API so the swap is a one-liner.
const StellarAssetDecimals = 7

// FormatAmount renders an integer base-unit amount as a decimal string
// with `decimals` fractional digits (e.g. 30000000, 7 → "3.0000000").
// The integer part is grouped with thin separators for readability.
func FormatAmount(base *big.Int, decimals uint32) string {
	if base == nil {
		return ""
	}
	neg := base.Sign() < 0
	abs := new(big.Int).Abs(base)
	if decimals == 0 {
		return sign(neg) + group(abs.String())
	}
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
	intPart := new(big.Int).Quo(abs, scale)
	frac := new(big.Int).Rem(abs, scale)
	fracStr := frac.String()
	for len(fracStr) < int(decimals) {
		fracStr = "0" + fracStr
	}
	return sign(neg) + group(intPart.String()) + "." + fracStr
}

func sign(neg bool) string {
	if neg {
		return "-"
	}
	return ""
}

// group inserts commas every three digits from the right.
func group(s string) string {
	n := len(s)
	if n <= 3 {
		return s
	}
	var b strings.Builder
	pre := n % 3
	if pre > 0 {
		b.WriteString(s[:pre])
		if n > pre {
			b.WriteByte(',')
		}
	}
	for i := pre; i < n; i += 3 {
		b.WriteString(s[i : i+3])
		if i+3 < n {
			b.WriteByte(',')
		}
	}
	return b.String()
}
