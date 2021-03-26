package legacytx

import (
	"github.com/line/lbm-sdk/v2/codec"
)

func RegisterLegacyAminoCodec(cdc *codec.LegacyAmino) {
	cdc.RegisterConcrete(StdTx{}, "lbm-sdk/StdTx", nil)
}