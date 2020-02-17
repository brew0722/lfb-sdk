package keeper

import (
	"fmt"
	"strconv"

	"github.com/cosmos/cosmos-sdk/codec"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/line/link/x/token/internal/types"
	"github.com/tendermint/tendermint/libs/log"
)

type Keeper struct {
	supplyKeeper  types.SupplyKeeper
	iamKeeper     types.IamKeeper
	accountKeeper types.AccountKeeper
	bankKeeper    types.BankKeeper
	storeKey      sdk.StoreKey
	cdc           *codec.Codec
}

func NewKeeper(cdc *codec.Codec, supplyKeeper types.SupplyKeeper, iamKeeper types.IamKeeper, accountKeeper types.AccountKeeper, bankKeeper types.BankKeeper, storeKey sdk.StoreKey) Keeper {
	return Keeper{
		supplyKeeper:  supplyKeeper,
		iamKeeper:     iamKeeper.WithPrefix(types.ModuleName),
		accountKeeper: accountKeeper,
		bankKeeper:    bankKeeper,
		storeKey:      storeKey,
		cdc:           cdc,
	}
}

func (k Keeper) Logger(ctx sdk.Context) log.Logger {
	return ctx.Logger().With("module", fmt.Sprintf("x/%s", types.ModuleName))
}

func (k Keeper) IssueFT(ctx sdk.Context, token types.FT, amount sdk.Int, owner sdk.AccAddress) sdk.Error {
	err := k.setToken(ctx, token)
	if err != nil {
		return err
	}

	err = k.mintTokens(ctx, sdk.NewCoins(sdk.NewCoin(token.GetDenom(), amount)), owner)
	if err != nil {
		return err
	}

	modifyTokenURIPermission := types.NewModifyTokenURIPermission(token.GetDenom())
	k.AddPermission(ctx, owner, modifyTokenURIPermission)
	ctx.EventManager().EmitEvents(sdk.Events{
		sdk.NewEvent(
			types.EventTypeIssueToken,
			sdk.NewAttribute(types.AttributeKeyName, token.GetName()),
			sdk.NewAttribute(types.AttributeKeySymbol, token.GetSymbol()),
			sdk.NewAttribute(types.AttributeKeyTokenID, token.GetTokenID()),
			sdk.NewAttribute(types.AttributeKeyOwner, owner.String()),
			sdk.NewAttribute(types.AttributeKeyAmount, amount.String()),
			sdk.NewAttribute(types.AttributeKeyMintable, strconv.FormatBool(token.GetMintable())),
			sdk.NewAttribute(types.AttributeKeyDecimals, token.GetDecimals().String()),
		),
		sdk.NewEvent(
			types.EventTypeGrantPermToken,
			sdk.NewAttribute(types.AttributeKeyTo, owner.String()),
			sdk.NewAttribute(types.AttributeKeyResource, modifyTokenURIPermission.GetResource()),
			sdk.NewAttribute(types.AttributeKeyAction, modifyTokenURIPermission.GetAction()),
		),
	})

	if token.GetMintable() {
		mintPerm := types.NewMintPermission(token.GetDenom())
		k.AddPermission(ctx, owner, mintPerm)
		burnPerm := types.NewBurnPermission(token.GetDenom())
		k.AddPermission(ctx, owner, burnPerm)
		ctx.EventManager().EmitEvents(sdk.Events{
			sdk.NewEvent(
				types.EventTypeGrantPermToken,
				sdk.NewAttribute(types.AttributeKeyTo, owner.String()),
				sdk.NewAttribute(types.AttributeKeyResource, mintPerm.GetResource()),
				sdk.NewAttribute(types.AttributeKeyAction, mintPerm.GetAction()),
			),
			sdk.NewEvent(
				types.EventTypeGrantPermToken,
				sdk.NewAttribute(types.AttributeKeyTo, owner.String()),
				sdk.NewAttribute(types.AttributeKeyResource, burnPerm.GetResource()),
				sdk.NewAttribute(types.AttributeKeyAction, burnPerm.GetAction()),
			),
		})
	}

	return nil
}

func (k Keeper) ModifyTokenURI(ctx sdk.Context, owner sdk.AccAddress, symbol, tokenID, tokenURI string) sdk.Error {
	token, err := k.GetToken(ctx, symbol, tokenID)
	if err != nil {
		return err
	}
	tokenURIModifyPerm := types.NewModifyTokenURIPermission(token.GetDenom())
	if !k.HasPermission(ctx, owner, tokenURIModifyPerm) {
		return types.ErrTokenNoPermission(types.DefaultCodespace, owner, tokenURIModifyPerm)
	}
	token.SetTokenURI(tokenURI)

	err = k.ModifyToken(ctx, token)
	if err != nil {
		return err
	}

	ctx.EventManager().EmitEvents(sdk.Events{
		sdk.NewEvent(
			types.EventTypeModifyTokenURI,
			sdk.NewAttribute(types.AttributeKeyName, token.GetName()),
			sdk.NewAttribute(types.AttributeKeySymbol, token.GetSymbol()),
			sdk.NewAttribute(types.AttributeKeyTokenID, token.GetTokenID()),
			sdk.NewAttribute(types.AttributeKeyOwner, owner.String()),
			sdk.NewAttribute(types.AttributeKeyTokenURI, token.GetTokenURI()),
		),
	})
	return nil
}
