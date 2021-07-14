package simulation_test

import (
	"math/big"

	sdk "github.com/line/lfb-sdk/types"
)

func init() {
	sdk.PowerReduction = sdk.NewIntFromBigInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil))
}