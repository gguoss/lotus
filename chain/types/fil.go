package types

import (
	"fmt"
	"math/big"
	"strings"

	"github.com/filecoin-project/lotus/build"
)

type FIL BigInt

func (f FIL) String() string {
	r := new(big.Rat).SetFrac(f.Int, big.NewInt(build.FilecoinPrecision))
	return strings.TrimRight(r.FloatString(18), "0.")
}

func ParseFIL(s string) (FIL, error) {
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return FIL{}, fmt.Errorf("failed to parse %q as a decimal number", s)
	}

	r = r.Mul(r, big.NewRat(build.FilecoinPrecision, 1))
	if !r.IsInt() {
		return FIL{}, fmt.Errorf("invalid FIL value: %q", s)
	}

	return FIL{r.Num()}, nil
}
