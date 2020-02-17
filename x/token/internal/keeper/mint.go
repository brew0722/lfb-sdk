package keeper

import (
	"fmt"

	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/line/link/x/token/internal/types"
)

func (k Keeper) MintTokens(ctx sdk.Context, amount sdk.Coins, from, to sdk.AccAddress) sdk.Error {
	for _, coin := range amount {
		token, err := k.GetToken(ctx, coin.Denom, "")
		if err != nil {
			return err
		}
		if err := k.isMintable(ctx, token, from); err != nil {
			return err
		}
	}
	err := k.mintTokens(ctx, amount, to)
	if err != nil {
		return err
	}
	ctx.EventManager().EmitEvents(sdk.Events{
		sdk.NewEvent(
			types.EventTypeMintToken,
			sdk.NewAttribute(types.AttributeKeyAmount, amount.String()),
			sdk.NewAttribute(types.AttributeKeyFrom, from.String()),
			sdk.NewAttribute(types.AttributeKeyTo, to.String()),
		),
	})
	return nil
}

func (k Keeper) isMintable(ctx sdk.Context, token types.Token, from sdk.AccAddress) sdk.Error {
	ft, ok := token.(types.FT)
	if !ok {
		return types.ErrTokenNotMintable(types.DefaultCodespace, token.GetDenom())
	}

	if !ft.GetMintable() {
		return types.ErrTokenNotMintable(types.DefaultCodespace, ft.GetDenom())
	}
	perm := types.NewMintPermission(ft.GetDenom())
	if !k.HasPermission(ctx, from, perm) {
		return types.ErrTokenNoPermission(types.DefaultCodespace, from, perm)
	}
	return nil
}

func (k Keeper) mintTokens(ctx sdk.Context, amount sdk.Coins, to sdk.AccAddress) sdk.Error {
	err := k.supplyKeeper.MintCoins(ctx, types.ModuleName, amount)
	if err != nil {
		return err
	}

	moduleAddr := k.supplyKeeper.GetModuleAddress(types.ModuleName)
	if moduleAddr == nil {
		return sdk.ErrUnknownAddress(fmt.Sprintf("module account %s does not exist", types.ModuleName))
	}

	_, err = k.bankKeeper.SubtractCoins(ctx, moduleAddr, amount)
	if err != nil {
		return err
	}

	_, err = k.bankKeeper.AddCoins(ctx, to, amount)
	if err != nil {
		return err
	}

	return nil
}

func (k Keeper) BurnTokens(ctx sdk.Context, amount sdk.Coins, from sdk.AccAddress) sdk.Error {
	err := k.isBurnable(ctx, from, from, amount)
	if err != nil {
		return sdk.ErrInsufficientCoins(fmt.Sprintf("%v has not enough coins for %v", from, amount))
	}

	err = k.burnTokens(ctx, from, amount)
	if err != nil {
		return err
	}
	ctx.EventManager().EmitEvents(sdk.Events{
		sdk.NewEvent(
			types.EventTypeBurnToken,
			sdk.NewAttribute(types.AttributeKeyAmount, amount.String()),
			sdk.NewAttribute(types.AttributeKeyFrom, from.String()),
		),
	})
	return nil
}

func (k Keeper) isBurnable(ctx sdk.Context, permissionOwner, tokenOwner sdk.AccAddress, amount sdk.Coins) sdk.Error {
	if !k.hasEnoughCoins(ctx, tokenOwner, amount) {
		return sdk.ErrInsufficientCoins(fmt.Sprintf("%v has not enough coins for %v", tokenOwner, amount))
	}

	for _, coin := range amount {
		perm := types.NewBurnPermission(coin.Denom)
		if !k.HasPermission(ctx, permissionOwner, perm) {
			return types.ErrTokenNoPermission(types.DefaultCodespace, permissionOwner, perm)
		}
	}
	return nil
}

func (k Keeper) hasEnoughCoins(ctx sdk.Context, from sdk.AccAddress, amount sdk.Coins) bool {
	return k.accountKeeper.GetAccount(ctx, from).GetCoins().IsAllGTE(amount)
}

func (k Keeper) burnTokens(ctx sdk.Context, from sdk.AccAddress, amount sdk.Coins) sdk.Error {
	moduleAddr := k.supplyKeeper.GetModuleAddress(types.ModuleName)
	if moduleAddr == nil {
		return sdk.ErrUnknownAddress(fmt.Sprintf("module account %s does not exist", types.ModuleName))
	}

	_, err := k.bankKeeper.SubtractCoins(ctx, from, amount)
	if err != nil {
		return err
	}

	_, err = k.bankKeeper.AddCoins(ctx, moduleAddr, amount)
	if err != nil {
		return err
	}

	err = k.supplyKeeper.BurnCoins(ctx, types.ModuleName, amount)
	if err != nil {
		return err
	}
	return nil
}
