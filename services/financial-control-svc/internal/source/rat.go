package source

import (
	"math/big"
	"strings"
)

func domainRat(s string) (*big.Rat, bool) {
	return new(big.Rat).SetString(strings.TrimSpace(s))
}
