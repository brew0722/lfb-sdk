package keeper

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"path/filepath"

	"github.com/line/lfb-sdk/codec"
	"github.com/line/lfb-sdk/store/prefix"
	sdk "github.com/line/lfb-sdk/types"
	sdkerrors "github.com/line/lfb-sdk/types/errors"
	authkeeper "github.com/line/lfb-sdk/x/auth/keeper"
	paramtypes "github.com/line/lfb-sdk/x/params/types"
	"github.com/line/lfb-sdk/x/wasm/internal/types"
	abci "github.com/line/ostracon/abci/types"
	"github.com/line/ostracon/crypto"
	"github.com/line/ostracon/libs/log"
	wasmvm "github.com/line/wasmvm"
	wasmvmtypes "github.com/line/wasmvm/types"
)

// MaxGas was restriction is increased to 0x7FFFFFFFFFFFFFFF in CosmWasm 0.10.
// This const is not needed now and we can manage it in another way.
const MaxGas = 10_000_000_000

// contractMemoryLimit is the memory limit of each contract execution (in MiB)
// constant value so all nodes run with the same limit.
const contractMemoryLimit = 32

// Option is an extension point to instantiate keeper with non default values
type Option interface {
	apply(*Keeper)
}

type messenger interface {
	DispatchMsg(ctx sdk.Context, contractAddr sdk.AccAddress, contractIBCPortID string, msg wasmvmtypes.CosmosMsg) (events []sdk.Event, data [][]byte, err error)
}

type coinTransferrer interface {
	// TransferCoins sends the coin amounts from the source to the destination with rules applied.
	TransferCoins(ctx sdk.Context, fromAddr sdk.AccAddress, toAddr sdk.AccAddress, amt sdk.Coins) error
}

// Keeper will have a reference to Wasmer with it's own data directory.
type Keeper struct {
	storeKey         sdk.StoreKey
	cdc              codec.Marshaler
	accountKeeper    types.AccountKeeper
	bank             coinTransferrer
	ChannelKeeper    types.ChannelKeeper
	portKeeper       types.PortKeeper
	capabilityKeeper types.CapabilityKeeper

	wasmer       types.WasmerEngine
	queryPlugins QueryPlugins
	messenger    messenger
	// queryGasLimit is the max wasmvm gas that can be spent on executing a query with a contract
	queryGasLimit uint64
	authZPolicy   AuthorizationPolicy
	paramSpace    *paramtypes.Subspace
}

// NewKeeper creates a new contract Keeper instance
// If customEncoders is non-nil, we can use this to override some of the message handler, especially custom
func NewKeeper(
	cdc codec.Marshaler,
	storeKey sdk.StoreKey,
	paramSpace *paramtypes.Subspace,
	accountKeeper authkeeper.AccountKeeper,
	bankKeeper types.BankKeeper,
	stakingKeeper types.StakingKeeper,
	distKeeper types.DistributionKeeper,
	channelKeeper types.ChannelKeeper,
	portKeeper types.PortKeeper,
	capabilityKeeper types.CapabilityKeeper,
	router sdk.Router,
	encodeRouter types.Router,
	queryRouter GRPCQueryRouter,
	homeDir string,
	wasmConfig types.WasmConfig,
	supportedFeatures string,
	customEncoders *MessageEncoders,
	customPlugins *QueryPlugins,
	opts ...Option,
) Keeper {
	wasmer, err := wasmvm.NewVM(filepath.Join(homeDir, "wasm"), supportedFeatures, contractMemoryLimit, wasmConfig.ContractDebugMode, wasmConfig.MemoryCacheSize)
	if err != nil {
		panic(err)
	}
	// set KeyTable if it has not already been set
	if !paramSpace.HasKeyTable() {
		paramSpace = paramSpace.WithKeyTable(types.ParamKeyTable())
	}

	keeper := Keeper{
		storeKey:         storeKey,
		cdc:              cdc,
		wasmer:           wasmer,
		accountKeeper:    accountKeeper,
		bank:             NewBankCoinTransferrer(bankKeeper),
		ChannelKeeper:    channelKeeper,
		portKeeper:       portKeeper,
		capabilityKeeper: capabilityKeeper,
		messenger:        NewDefaultMessageHandler(router, encodeRouter, channelKeeper, capabilityKeeper, cdc, customEncoders),
		queryGasLimit:    wasmConfig.SmartQueryGasLimit,
		authZPolicy:      DefaultAuthorizationPolicy{},
		paramSpace:       paramSpace,
	}
	keeper.queryPlugins = DefaultQueryPlugins(bankKeeper, stakingKeeper, distKeeper, channelKeeper, queryRouter, &keeper).Merge(customPlugins)
	for _, o := range opts {
		o.apply(&keeper)
	}
	return keeper
}

func (k Keeper) getUploadAccessConfig(ctx sdk.Context) types.AccessConfig {
	var a types.AccessConfig
	k.paramSpace.Get(ctx, types.ParamStoreKeyUploadAccess, &a)
	return a
}

func (k Keeper) getInstantiateAccessConfig(ctx sdk.Context) types.AccessType {
	var a types.AccessType
	k.paramSpace.Get(ctx, types.ParamStoreKeyInstantiateAccess, &a)
	return a
}

func (k Keeper) getContractStatusAccessConfig(ctx sdk.Context) types.AccessConfig {
	var a types.AccessConfig
	k.paramSpace.Get(ctx, types.ParamStoreKeyContractStatusAccess, &a)
	return a
}

func (k Keeper) getMaxWasmCodeSize(ctx sdk.Context) uint64 {
	var a uint64
	k.paramSpace.Get(ctx, types.ParamStoreKeyMaxWasmCodeSize, &a)
	return a
}

func (k Keeper) getGasMultiplier(ctx sdk.Context) uint64 {
	var a uint64
	k.paramSpace.Get(ctx, types.ParamStoreKeyGasMultiplier, &a)
	return a
}

func (k Keeper) getInstanceCost(ctx sdk.Context) uint64 {
	var a uint64
	k.paramSpace.Get(ctx, types.ParamStoreKeyInstanceCost, &a)
	return a
}

func (k Keeper) getCompileCost(ctx sdk.Context) uint64 {
	var a uint64
	k.paramSpace.Get(ctx, types.ParamStoreKeyCompileCost, &a)
	return a
}

// GetParams returns the total set of wasm parameters.
func (k Keeper) GetParams(ctx sdk.Context) types.Params {
	var params types.Params
	k.paramSpace.GetParamSet(ctx, &params)
	return params
}

func (k Keeper) setParams(ctx sdk.Context, ps types.Params) {
	k.paramSpace.SetParamSet(ctx, &ps)
}

// Create uploads and compiles a WASM contract, returning a short identifier for the contract
func (k Keeper) Create(ctx sdk.Context, creator sdk.AccAddress, wasmCode []byte, source string, builder string, instantiateAccess *types.AccessConfig) (codeID uint64, err error) {
	return k.create(ctx, creator, wasmCode, source, builder, instantiateAccess, k.authZPolicy)
}

func (k Keeper) create(ctx sdk.Context, creator sdk.AccAddress, wasmCode []byte, source string, builder string, instantiateAccess *types.AccessConfig, authZ AuthorizationPolicy) (codeID uint64, err error) {
	if !authZ.CanCreateCode(k.getUploadAccessConfig(ctx), creator) {
		return 0, sdkerrors.Wrap(sdkerrors.ErrUnauthorized, "can not create code")
	}
	wasmCode, err = uncompress(wasmCode, k.getMaxWasmCodeSize(ctx))
	if err != nil {
		return 0, sdkerrors.Wrap(types.ErrCreateFailed, err.Error())
	}
	ctx.GasMeter().ConsumeGas(k.getCompileCost(ctx)*uint64(len(wasmCode)), "Compiling WASM Bytecode")

	codeHash, err := k.wasmer.Create(wasmCode)
	if err != nil {
		return 0, sdkerrors.Wrap(types.ErrCreateFailed, err.Error())
	}
	codeID = k.autoIncrementID(ctx, types.KeyLastCodeID)
	if instantiateAccess == nil {
		defaultAccessConfig := k.getInstantiateAccessConfig(ctx).With(creator)
		instantiateAccess = &defaultAccessConfig
	}
	codeInfo := types.NewCodeInfo(codeHash, creator, source, builder, *instantiateAccess)
	k.storeCodeInfo(ctx, codeID, codeInfo)
	return codeID, nil
}

func (k Keeper) storeCodeInfo(ctx sdk.Context, codeID uint64, codeInfo types.CodeInfo) {
	store := ctx.KVStore(k.storeKey)
	// 0x01 | codeID (uint64) -> ContractInfo
	store.Set(types.GetCodeKey(codeID), k.cdc.MustMarshalBinaryBare(&codeInfo))
}

func (k Keeper) importCode(ctx sdk.Context, codeID uint64, codeInfo types.CodeInfo, wasmCode []byte) error {
	wasmCode, err := uncompress(wasmCode, k.getMaxWasmCodeSize(ctx))
	if err != nil {
		return sdkerrors.Wrap(types.ErrCreateFailed, err.Error())
	}
	newCodeHash, err := k.wasmer.Create(wasmCode)
	if err != nil {
		return sdkerrors.Wrap(types.ErrCreateFailed, err.Error())
	}
	if !bytes.Equal(codeInfo.CodeHash, newCodeHash) {
		return sdkerrors.Wrap(types.ErrInvalid, "code hashes not same")
	}

	store := ctx.KVStore(k.storeKey)
	key := types.GetCodeKey(codeID)
	if store.Has(key) {
		return sdkerrors.Wrapf(types.ErrDuplicate, "duplicate code: %d", codeID)
	}
	// 0x01 | codeID (uint64) -> ContractInfo
	store.Set(key, k.cdc.MustMarshalBinaryBare(&codeInfo))
	return nil
}

// Instantiate creates an instance of a WASM contract
func (k Keeper) Instantiate(ctx sdk.Context, codeID uint64, creator, admin sdk.AccAddress, initMsg []byte, label string, deposit sdk.Coins) (sdk.AccAddress, []byte, error) {
	return k.instantiate(ctx, codeID, creator, admin, initMsg, label, deposit, k.authZPolicy)
}

func (k Keeper) instantiate(ctx sdk.Context, codeID uint64, creator, admin sdk.AccAddress, initMsg []byte, label string, deposit sdk.Coins, authZ AuthorizationPolicy) (sdk.AccAddress, []byte, error) {
	if !k.IsPinnedCode(ctx, codeID) {
		ctx.GasMeter().ConsumeGas(k.getInstanceCost(ctx), "Loading CosmWasm module: instantiate")
	}

	// create contract address
	contractAddress := k.generateContractAddress(ctx, codeID)
	existingAcct := k.accountKeeper.GetAccount(ctx, contractAddress)
	if existingAcct != nil {
		return nil, nil, sdkerrors.Wrap(types.ErrAccountExists, existingAcct.GetAddress().String())
	}

	// deposit initial contract funds
	if !deposit.IsZero() {
		if err := k.bank.TransferCoins(ctx, creator, contractAddress, deposit); err != nil {
			return nil, nil, err
		}

	} else {
		// create an empty account (so we don't have issues later)
		// TODO: can we remove this?
		contractAccount := k.accountKeeper.NewAccountWithAddress(ctx, contractAddress)
		k.accountKeeper.SetAccount(ctx, contractAccount)
	}

	// get contact info
	store := ctx.KVStore(k.storeKey)
	bz := store.Get(types.GetCodeKey(codeID))
	if bz == nil {
		return nil, nil, sdkerrors.Wrap(types.ErrNotFound, "code")
	}
	var codeInfo types.CodeInfo
	k.cdc.MustUnmarshalBinaryBare(bz, &codeInfo)

	if !authZ.CanInstantiateContract(codeInfo.InstantiateConfig, creator) {
		return nil, nil, sdkerrors.Wrap(sdkerrors.ErrUnauthorized, "can not instantiate")
	}

	// prepare params for contract instantiate call
	env := types.NewEnv(ctx, contractAddress)
	info := types.NewInfo(creator, deposit)

	// create prefixed data store
	// 0x03 | contractAddress (sdk.AccAddress)
	prefixStoreKey := types.GetContractStorePrefix(contractAddress)
	prefixStore := prefix.NewStore(ctx.KVStore(k.storeKey), prefixStoreKey)
	wasmStore := types.NewWasmStore(prefixStore)

	// prepare querier
	querier := NewQueryHandler(ctx, k.queryPlugins, contractAddress, k.getGasMultiplier(ctx))

	// instantiate wasm contract
	gas := gasForContract(ctx, k.getGasMultiplier(ctx))
	res, gasUsed, err := k.wasmer.Instantiate(codeInfo.CodeHash, env, info, initMsg, wasmStore, k.cosmwasmAPI(ctx), querier, k.gasMeter(ctx), gas)
	k.consumeGas(ctx, gasUsed)
	if err != nil {
		return contractAddress, nil, sdkerrors.Wrap(types.ErrInstantiateFailed, err.Error())
	}

	// emit all events from this contract itself
	events := types.ParseEvents(res.Attributes, contractAddress)
	ctx.EventManager().EmitEvents(events)

	// persist instance first
	createdAt := types.NewAbsoluteTxPosition(ctx)
	contractInfo := types.NewContractInfo(codeID, creator, admin, label, createdAt, types.ContractStatusActive)

	// check for IBC flag
	report, err := k.wasmer.AnalyzeCode(codeInfo.CodeHash)
	if err != nil {
		return contractAddress, nil, sdkerrors.Wrap(types.ErrInstantiateFailed, err.Error())
	}
	if report.HasIBCEntryPoints {
		// register IBC port
		ibcPort, err := k.ensureIbcPort(ctx, contractAddress)
		if err != nil {
			return nil, nil, err
		}
		contractInfo.IBCPortID = ibcPort
	}

	// store contract before dispatch so that contract could be called back
	k.storeContractInfo(ctx, contractAddress, &contractInfo)
	k.appendToContractHistory(ctx, contractAddress, contractInfo.InitialHistory(initMsg))

	// dispatch submessages then messages
	err = k.dispatchAll(ctx, contractAddress, contractInfo.IBCPortID, res.Submessages, res.Messages)
	if err != nil {
		return nil, nil, sdkerrors.Wrap(err, "dispatch")
	}

	return contractAddress, res.Data, nil
}

// Execute executes the contract instance
func (k Keeper) Execute(ctx sdk.Context, contractAddress sdk.AccAddress, caller sdk.AccAddress, msg []byte, coins sdk.Coins) (*sdk.Result, error) {
	contractInfo, codeInfo, prefixStore, err := k.contractInstance(ctx, contractAddress)
	if err != nil {
		return nil, err
	}
	if contractInfo.Status != types.ContractStatusActive {
		return nil, sdkerrors.Wrap(types.ErrInvalid, "inactive contract")
	}

	if !k.IsPinnedCode(ctx, contractInfo.CodeID) {
		ctx.GasMeter().ConsumeGas(k.getInstanceCost(ctx), "Loading CosmWasm module: execute")
	}

	// add more funds
	if !coins.IsZero() {
		if err := k.bank.TransferCoins(ctx, caller, contractAddress, coins); err != nil {
			return nil, err
		}
	}

	env := types.NewEnv(ctx, contractAddress)
	info := types.NewInfo(caller, coins)

	// prepare querier
	querier := NewQueryHandler(ctx, k.queryPlugins, contractAddress, k.getGasMultiplier(ctx))
	gas := gasForContract(ctx, k.getGasMultiplier(ctx))
	wasmStore := types.NewWasmStore(prefixStore)
	res, gasUsed, execErr := k.wasmer.Execute(codeInfo.CodeHash, env, info, msg, wasmStore, k.cosmwasmAPI(ctx), querier, k.gasMeter(ctx), gas)
	k.consumeGas(ctx, gasUsed)
	if execErr != nil {
		return nil, sdkerrors.Wrap(types.ErrExecuteFailed, execErr.Error())
	}

	// emit all events from this contract itself
	events := types.ParseEvents(res.Attributes, contractAddress)
	ctx.EventManager().EmitEvents(events)

	// dispatch submessages then messages
	err = k.dispatchAll(ctx, contractAddress, contractInfo.IBCPortID, res.Submessages, res.Messages)
	if err != nil {
		return nil, sdkerrors.Wrap(err, "dispatch")
	}

	return &sdk.Result{
		Data: res.Data,
	}, nil
}

// Migrate allows to upgrade a contract to a new code with data migration.
func (k Keeper) Migrate(ctx sdk.Context, contractAddress sdk.AccAddress, caller sdk.AccAddress, newCodeID uint64, msg []byte) (*sdk.Result, error) {
	return k.migrate(ctx, contractAddress, caller, newCodeID, msg, k.authZPolicy)
}

func (k Keeper) migrate(ctx sdk.Context, contractAddress sdk.AccAddress, caller sdk.AccAddress, newCodeID uint64, msg []byte, authZ AuthorizationPolicy) (*sdk.Result, error) {
	if !k.IsPinnedCode(ctx, newCodeID) {
		ctx.GasMeter().ConsumeGas(k.getInstanceCost(ctx), "Loading CosmWasm module: migrate")
	}

	contractInfo := k.GetContractInfo(ctx, contractAddress)
	if contractInfo == nil {
		return nil, sdkerrors.Wrap(sdkerrors.ErrInvalidRequest, "unknown contract")
	}
	if contractInfo.Status != types.ContractStatusActive {
		return nil, sdkerrors.Wrap(types.ErrInvalid, "inactive contract")
	}
	if !authZ.CanModifyContract(contractInfo.AdminAddr(), caller) {
		return nil, sdkerrors.Wrap(sdkerrors.ErrUnauthorized, "can not migrate")
	}

	newCodeInfo := k.GetCodeInfo(ctx, newCodeID)
	if newCodeInfo == nil {
		return nil, sdkerrors.Wrap(sdkerrors.ErrInvalidRequest, "unknown code")
	}

	// check for IBC flag
	switch report, err := k.wasmer.AnalyzeCode(newCodeInfo.CodeHash); {
	case err != nil:
		return nil, sdkerrors.Wrap(types.ErrMigrationFailed, err.Error())
	case !report.HasIBCEntryPoints && contractInfo.IBCPortID != "":
		// prevent update to non ibc contract
		return nil, sdkerrors.Wrap(types.ErrMigrationFailed, "requires ibc callbacks")
	case report.HasIBCEntryPoints && contractInfo.IBCPortID == "":
		// add ibc port
		ibcPort, err := k.ensureIbcPort(ctx, contractAddress)
		if err != nil {
			return nil, err
		}
		contractInfo.IBCPortID = ibcPort
	}

	env := types.NewEnv(ctx, contractAddress)

	// prepare querier
	querier := NewQueryHandler(ctx, k.queryPlugins, contractAddress, k.getGasMultiplier(ctx))

	prefixStoreKey := types.GetContractStorePrefix(contractAddress)
	prefixStore := prefix.NewStore(ctx.KVStore(k.storeKey), prefixStoreKey)
	wasmStore := types.NewWasmStore(prefixStore)
	gas := gasForContract(ctx, k.getGasMultiplier(ctx))
	res, gasUsed, err := k.wasmer.Migrate(newCodeInfo.CodeHash, env, msg, &wasmStore, k.cosmwasmAPI(ctx), &querier, k.gasMeter(ctx), gas)
	k.consumeGas(ctx, gasUsed)
	if err != nil {
		return nil, sdkerrors.Wrap(types.ErrMigrationFailed, err.Error())
	}

	// emit all events from this contract migration itself
	events := types.ParseEvents(res.Attributes, contractAddress)
	ctx.EventManager().EmitEvents(events)

	// delete old secondary index entry
	k.deleteContractSecondIndex(ctx, contractAddress, contractInfo)
	// persist migration updates
	historyEntry := contractInfo.AddMigration(ctx, newCodeID, msg)
	k.appendToContractHistory(ctx, contractAddress, historyEntry)
	k.storeContractInfo(ctx, contractAddress, contractInfo)

	// dispatch submessages then messages
	err = k.dispatchAll(ctx, contractAddress, contractInfo.IBCPortID, res.Submessages, res.Messages)
	if err != nil {
		return nil, sdkerrors.Wrap(err, "dispatch")
	}

	return &sdk.Result{
		Data: res.Data,
	}, nil
}

// Sudo allows priviledged access to a contract. This can never be called by governance or external tx, but only by
// another native Go module directly. Thus, the keeper doesn't place any access controls on it, that is the
// responsibility or the app developer (who passes the wasm.Keeper in app.go)
func (k Keeper) Sudo(ctx sdk.Context, contractAddress sdk.AccAddress, msg []byte) (*sdk.Result, error) {
	contractInfo, codeInfo, prefixStore, err := k.contractInstance(ctx, contractAddress)
	if err != nil {
		return nil, err
	}

	if !k.IsPinnedCode(ctx, contractInfo.CodeID) {
		ctx.GasMeter().ConsumeGas(k.getInstanceCost(ctx), "Loading CosmWasm module: sudo")
	}

	env := types.NewEnv(ctx, contractAddress)

	// prepare querier
	querier := NewQueryHandler(ctx, k.queryPlugins, contractAddress, k.getGasMultiplier(ctx))
	gas := gasForContract(ctx, k.getGasMultiplier(ctx))
	wasmStore := types.NewWasmStore(prefixStore)
	res, gasUsed, execErr := k.wasmer.Sudo(codeInfo.CodeHash, env, msg, wasmStore, k.cosmwasmAPI(ctx), querier, k.gasMeter(ctx), gas)
	k.consumeGas(ctx, gasUsed)
	if execErr != nil {
		return nil, sdkerrors.Wrap(types.ErrExecuteFailed, execErr.Error())
	}

	// emit all events from this contract itself
	events := types.ParseEvents(res.Attributes, contractAddress)
	ctx.EventManager().EmitEvents(events)

	// dispatch submessages then messages
	err = k.dispatchAll(ctx, contractAddress, contractInfo.IBCPortID, res.Submessages, res.Messages)
	if err != nil {
		return nil, sdkerrors.Wrap(err, "dispatch")
	}

	return &sdk.Result{
		Data: res.Data,
	}, nil
}

// reply is only called from keeper internal functions (dispatchSubmessages) after processing the submessage
// it
func (k Keeper) reply(ctx sdk.Context, contractAddress sdk.AccAddress, reply wasmvmtypes.Reply) (*sdk.Result, error) {
	contractInfo, codeInfo, prefixStore, err := k.contractInstance(ctx, contractAddress)
	if err != nil {
		return nil, err
	}

	// current thought is to charge gas like a fresh run, we can revisit whether to give it a discount later
	if !k.IsPinnedCode(ctx, contractInfo.CodeID) {
		ctx.GasMeter().ConsumeGas(k.getInstanceCost(ctx), "Loading CosmWasm module: reply")
	}

	env := types.NewEnv(ctx, contractAddress)

	// prepare querier
	querier := QueryHandler{
		Ctx:           ctx,
		Plugins:       k.queryPlugins,
		GasMultiplier: k.getGasMultiplier(ctx),
	}
	gas := gasForContract(ctx, k.getGasMultiplier(ctx))
	wasmStore := types.NewWasmStore(prefixStore)
	res, gasUsed, execErr := k.wasmer.Reply(codeInfo.CodeHash, env, reply, wasmStore, k.cosmwasmAPI(ctx), querier, k.gasMeter(ctx), gas)
	k.consumeGas(ctx, gasUsed)
	if execErr != nil {
		return nil, sdkerrors.Wrap(types.ErrExecuteFailed, execErr.Error())
	}

	// emit all events from this contract itself
	events := types.ParseEvents(res.Attributes, contractAddress)
	ctx.EventManager().EmitEvents(events)

	// dispatch submessages then messages
	err = k.dispatchAll(ctx, contractAddress, contractInfo.IBCPortID, res.Submessages, res.Messages)
	if err != nil {
		return nil, sdkerrors.Wrap(err, "dispatch")
	}

	return &sdk.Result{
		Data: res.Data,
	}, nil
}

func (k Keeper) deleteContractSecondIndex(ctx sdk.Context, contractAddress sdk.AccAddress, contractInfo *types.ContractInfo) {
	ctx.KVStore(k.storeKey).Delete(types.GetContractByCreatedSecondaryIndexKey(contractAddress, contractInfo))
}

// UpdateContractAdmin sets the admin value on the ContractInfo. It must be a valid address (use ClearContractAdmin to remove it)
func (k Keeper) UpdateContractAdmin(ctx sdk.Context, contractAddress sdk.AccAddress, caller sdk.AccAddress, newAdmin sdk.AccAddress) error {
	return k.setContractAdmin(ctx, contractAddress, caller, newAdmin, k.authZPolicy)
}

// ClearContractAdmin sets the admin value on the ContractInfo to nil, to disable further migrations/ updates.
func (k Keeper) ClearContractAdmin(ctx sdk.Context, contractAddress sdk.AccAddress, caller sdk.AccAddress) error {
	return k.setContractAdmin(ctx, contractAddress, caller, nil, k.authZPolicy)
}

// UpdateContractStatus sets a new status of the contract on the ContractInfo.
func (k Keeper) UpdateContractStatus(ctx sdk.Context, contractAddress sdk.AccAddress, caller sdk.AccAddress, status types.ContractStatus) error {
	return k.updateContractStatus(ctx, contractAddress, caller, status, k.authZPolicy)
}

func (k Keeper) updateContractStatus(ctx sdk.Context, contractAddress sdk.AccAddress, caller sdk.AccAddress, status types.ContractStatus, authZ AuthorizationPolicy) error {
	if !authZ.CanUpdateContractStatus(k.getContractStatusAccessConfig(ctx), caller) {
		return sdkerrors.Wrap(sdkerrors.ErrUnauthorized, "can not update contract status")
	}

	contractInfo := k.GetContractInfo(ctx, contractAddress)
	if contractInfo == nil {
		return sdkerrors.Wrap(sdkerrors.ErrInvalidRequest, "unknown contract")
	}
	if contractInfo.Status != status {
		contractInfo.Status = status
		k.storeContractInfo(ctx, contractAddress, contractInfo)
	}
	return nil
}

func (k Keeper) setContractAdmin(ctx sdk.Context, contractAddress, caller, newAdmin sdk.AccAddress, authZ AuthorizationPolicy) error {
	contractInfo := k.GetContractInfo(ctx, contractAddress)
	if contractInfo == nil {
		return sdkerrors.Wrap(sdkerrors.ErrInvalidRequest, "unknown contract")
	}
	if contractInfo.Status != types.ContractStatusActive {
		return sdkerrors.Wrap(types.ErrInvalid, "inactive contract")
	}
	if !authZ.CanModifyContract(contractInfo.AdminAddr(), caller) {
		return sdkerrors.Wrap(sdkerrors.ErrUnauthorized, "can not modify contract")
	}
	contractInfo.Admin = newAdmin.String()
	k.storeContractInfo(ctx, contractAddress, contractInfo)
	return nil
}

func (k Keeper) appendToContractHistory(ctx sdk.Context, contractAddr sdk.AccAddress, newEntries ...types.ContractCodeHistoryEntry) {
	store := ctx.KVStore(k.storeKey)
	// find last element position
	var pos uint64
	prefixStore := prefix.NewStore(store, types.GetContractCodeHistoryElementPrefix(contractAddr))
	if iter := prefixStore.ReverseIterator(nil, nil); iter.Valid() {
		pos = sdk.BigEndianToUint64(iter.Value())
	}
	// then store with incrementing position
	for i := range newEntries {
		pos++
		key := types.GetContractCodeHistoryElementKey(contractAddr, pos)
		store.Set(key, k.cdc.MustMarshalBinaryBare(&newEntries[i]))
	}
}

func (k Keeper) GetContractHistory(ctx sdk.Context, contractAddr sdk.AccAddress) []types.ContractCodeHistoryEntry {
	prefixStore := prefix.NewStore(ctx.KVStore(k.storeKey), types.GetContractCodeHistoryElementPrefix(contractAddr))
	r := make([]types.ContractCodeHistoryEntry, 0)
	iter := prefixStore.Iterator(nil, nil)
	for ; iter.Valid(); iter.Next() {
		var e types.ContractCodeHistoryEntry
		k.cdc.MustUnmarshalBinaryBare(iter.Value(), &e)
		r = append(r, e)
	}
	return r
}

// QuerySmart queries the smart contract itself.
func (k Keeper) QuerySmart(ctx sdk.Context, contractAddr sdk.AccAddress, req []byte) ([]byte, error) {
	contractInfo, codeInfo, prefixStore, err := k.contractInstance(ctx, contractAddr)
	if err != nil {
		return nil, err
	}
	if !k.IsPinnedCode(ctx, contractInfo.CodeID) {
		ctx.GasMeter().ConsumeGas(k.getInstanceCost(ctx), "Loading CosmWasm module: query")
	}

	// prepare querier
	querier := NewQueryHandler(ctx, k.queryPlugins, contractAddr, k.getGasMultiplier(ctx))

	env := types.NewEnv(ctx, contractAddr)
	wasmStore := types.NewWasmStore(prefixStore)
	queryResult, gasUsed, qErr := k.wasmer.Query(codeInfo.CodeHash, env, req, wasmStore, k.cosmwasmAPI(ctx), querier, k.gasMeter(ctx), gasForContract(ctx, k.getGasMultiplier(ctx)))
	k.consumeGas(ctx, gasUsed)
	if qErr != nil {
		return nil, sdkerrors.Wrap(types.ErrQueryFailed, qErr.Error())
	}
	return queryResult, nil
}

// QueryRaw returns the contract's state for give key. Returns `nil` when key is `nil`.
func (k Keeper) QueryRaw(ctx sdk.Context, contractAddress sdk.AccAddress, key []byte) []byte {
	if key == nil {
		return nil
	}
	prefixStoreKey := types.GetContractStorePrefix(contractAddress)
	prefixStore := prefix.NewStore(ctx.KVStore(k.storeKey), prefixStoreKey)
	return prefixStore.Get(key)
}

func (k Keeper) contractInstance(ctx sdk.Context, contractAddress sdk.AccAddress) (types.ContractInfo, types.CodeInfo, prefix.Store, error) {
	store := ctx.KVStore(k.storeKey)

	contractBz := store.Get(types.GetContractAddressKey(contractAddress))
	if contractBz == nil {
		return types.ContractInfo{}, types.CodeInfo{}, prefix.Store{}, sdkerrors.Wrap(types.ErrNotFound, "contract")
	}
	var contractInfo types.ContractInfo
	k.cdc.MustUnmarshalBinaryBare(contractBz, &contractInfo)

	codeInfoBz := store.Get(types.GetCodeKey(contractInfo.CodeID))
	if codeInfoBz == nil {
		return contractInfo, types.CodeInfo{}, prefix.Store{}, sdkerrors.Wrap(types.ErrNotFound, "code info")
	}
	var codeInfo types.CodeInfo
	k.cdc.MustUnmarshalBinaryBare(codeInfoBz, &codeInfo)
	prefixStoreKey := types.GetContractStorePrefix(contractAddress)
	prefixStore := prefix.NewStore(ctx.KVStore(k.storeKey), prefixStoreKey)
	return contractInfo, codeInfo, prefixStore, nil
}

func (k Keeper) GetContractInfo(ctx sdk.Context, contractAddress sdk.AccAddress) *types.ContractInfo {
	store := ctx.KVStore(k.storeKey)
	var contract types.ContractInfo
	contractBz := store.Get(types.GetContractAddressKey(contractAddress))
	if contractBz == nil {
		return nil
	}
	k.cdc.MustUnmarshalBinaryBare(contractBz, &contract)
	return &contract
}

func (k Keeper) containsContractInfo(ctx sdk.Context, contractAddress sdk.AccAddress) bool {
	store := ctx.KVStore(k.storeKey)
	return store.Has(types.GetContractAddressKey(contractAddress))
}

func (k Keeper) storeContractInfo(ctx sdk.Context, contractAddress sdk.AccAddress, contract *types.ContractInfo) {
	store := ctx.KVStore(k.storeKey)
	store.Set(types.GetContractAddressKey(contractAddress), k.cdc.MustMarshalBinaryBare(contract))
	store.Set(types.GetContractByCreatedSecondaryIndexKey(contractAddress, contract), []byte{})
}

func (k Keeper) IterateContractInfo(ctx sdk.Context, cb func(sdk.AccAddress, types.ContractInfo) bool) {
	prefixStore := prefix.NewStore(ctx.KVStore(k.storeKey), types.ContractKeyPrefix)
	iter := prefixStore.Iterator(nil, nil)
	for ; iter.Valid(); iter.Next() {
		var contract types.ContractInfo
		k.cdc.MustUnmarshalBinaryBare(iter.Value(), &contract)
		// cb returns true to stop early
		if cb(iter.Key(), contract) {
			break
		}
	}
}

func (k Keeper) GetContractState(ctx sdk.Context, contractAddress sdk.AccAddress) sdk.Iterator {
	prefixStoreKey := types.GetContractStorePrefix(contractAddress)
	prefixStore := prefix.NewStore(ctx.KVStore(k.storeKey), prefixStoreKey)
	return prefixStore.Iterator(nil, nil)
}

func (k Keeper) importContractState(ctx sdk.Context, contractAddress sdk.AccAddress, models []types.Model) error {
	prefixStoreKey := types.GetContractStorePrefix(contractAddress)
	prefixStore := prefix.NewStore(ctx.KVStore(k.storeKey), prefixStoreKey)
	for _, model := range models {
		if model.Value == nil {
			model.Value = []byte{}
		}
		if prefixStore.Has(model.Key) {
			return sdkerrors.Wrapf(types.ErrDuplicate, "duplicate key: %x", model.Key)
		}
		prefixStore.Set(model.Key, model.Value)
	}
	return nil
}

func (k Keeper) GetCodeInfo(ctx sdk.Context, codeID uint64) *types.CodeInfo {
	store := ctx.KVStore(k.storeKey)
	var codeInfo types.CodeInfo
	codeInfoBz := store.Get(types.GetCodeKey(codeID))
	if codeInfoBz == nil {
		return nil
	}
	k.cdc.MustUnmarshalBinaryBare(codeInfoBz, &codeInfo)
	return &codeInfo
}

func (k Keeper) containsCodeInfo(ctx sdk.Context, codeID uint64) bool {
	store := ctx.KVStore(k.storeKey)
	return store.Has(types.GetCodeKey(codeID))
}

func (k Keeper) IterateCodeInfos(ctx sdk.Context, cb func(uint64, types.CodeInfo) bool) {
	prefixStore := prefix.NewStore(ctx.KVStore(k.storeKey), types.CodeKeyPrefix)
	iter := prefixStore.Iterator(nil, nil)
	for ; iter.Valid(); iter.Next() {
		var c types.CodeInfo
		k.cdc.MustUnmarshalBinaryBare(iter.Value(), &c)
		// cb returns true to stop early
		if cb(binary.BigEndian.Uint64(iter.Key()), c) {
			return
		}
	}
}

func (k Keeper) GetByteCode(ctx sdk.Context, codeID uint64) ([]byte, error) {
	store := ctx.KVStore(k.storeKey)
	var codeInfo types.CodeInfo
	codeInfoBz := store.Get(types.GetCodeKey(codeID))
	if codeInfoBz == nil {
		return nil, nil
	}
	k.cdc.MustUnmarshalBinaryBare(codeInfoBz, &codeInfo)
	return k.wasmer.GetCode(codeInfo.CodeHash)
}

// PinCode pins the wasm contract in wasmvm cache
func (k Keeper) PinCode(ctx sdk.Context, codeID uint64) error {
	codeInfo := k.GetCodeInfo(ctx, codeID)
	if codeInfo == nil {
		return sdkerrors.Wrap(types.ErrNotFound, "code info")
	}

	if err := k.wasmer.Pin(codeInfo.CodeHash); err != nil {
		return sdkerrors.Wrap(types.ErrPinContractFailed, err.Error())
	}
	store := ctx.KVStore(k.storeKey)
	// store 1 byte to not run into `nil` debugging issues
	store.Set(types.GetPinnedCodeIndexPrefix(codeID), []byte{1})
	return nil
}

// UnpinCode removes the wasm contract from wasmvm cache
func (k Keeper) UnpinCode(ctx sdk.Context, codeID uint64) error {
	codeInfo := k.GetCodeInfo(ctx, codeID)
	if codeInfo == nil {
		return sdkerrors.Wrap(types.ErrNotFound, "code info")
	}
	if err := k.wasmer.Unpin(codeInfo.CodeHash); err != nil {
		return sdkerrors.Wrap(types.ErrUnpinContractFailed, err.Error())
	}

	store := ctx.KVStore(k.storeKey)
	store.Delete(types.GetPinnedCodeIndexPrefix(codeID))
	return nil
}

// IsPinnedCode returns true when codeID is pinned in wasmvm cache
func (k Keeper) IsPinnedCode(ctx sdk.Context, codeID uint64) bool {
	store := ctx.KVStore(k.storeKey)
	return store.Has(types.GetPinnedCodeIndexPrefix(codeID))
}

// InitializePinnedCodes updates wasmvm to pin to cache all contracts marked as pinned
func (k Keeper) InitializePinnedCodes(ctx sdk.Context) error {
	store := prefix.NewStore(ctx.KVStore(k.storeKey), types.PinnedCodeIndexPrefix)
	iter := store.Iterator(nil, nil)
	for ; iter.Valid(); iter.Next() {
		codeInfo := k.GetCodeInfo(ctx, types.ParsePinnedCodeIndex(iter.Value()))
		if codeInfo == nil {
			return sdkerrors.Wrap(types.ErrNotFound, "code info")
		}
		if err := k.wasmer.Pin(codeInfo.CodeHash); err != nil {
			return sdkerrors.Wrap(types.ErrPinContractFailed, err.Error())
		}
	}
	return nil
}

func (k Keeper) dispatchAll(ctx sdk.Context, contractAddr sdk.AccAddress, ibcPort string, subMsgs []wasmvmtypes.SubMsg, msgs []wasmvmtypes.CosmosMsg) error {
	// first dispatch all submessages (and the replies).
	err := k.dispatchSubmessages(ctx, contractAddr, ibcPort, subMsgs)
	if err != nil {
		return err
	}
	// then dispatch all the normal messages
	return k.dispatchMessages(ctx, contractAddr, ibcPort, msgs)
}

func (k Keeper) dispatchMessages(ctx sdk.Context, contractAddr sdk.AccAddress, ibcPort string, msgs []wasmvmtypes.CosmosMsg) error {
	for _, msg := range msgs {
		events, _, err := k.messenger.DispatchMsg(ctx, contractAddr, ibcPort, msg)
		if err != nil {
			return err
		}
		// redispatch all events, (type sdk.EventTypeMessage will be filtered out in the handler)
		ctx.EventManager().EmitEvents(events)
	}
	return nil
}

func (k Keeper) dispatchMsgWithGasLimit(ctx sdk.Context, contractAddr sdk.AccAddress, ibcPort string, msg wasmvmtypes.CosmosMsg, gasLimit uint64) (events []sdk.Event, data [][]byte, err error) {
	limitedMeter := sdk.NewGasMeter(gasLimit)
	subCtx := ctx.WithGasMeter(limitedMeter)

	// catch out of gas panic and just charge the entire gas limit
	defer func() {
		if r := recover(); r != nil {
			// if it's not an OutOfGas error, raise it again
			if _, ok := r.(sdk.ErrorOutOfGas); !ok {
				// log it to get the original stack trace somewhere (as panic(r) keeps message but stacktrace to here
				k.Logger(ctx).Info("SubMsg rethrowing panic: %#v", r)
				panic(r)
			}
			ctx.GasMeter().ConsumeGas(gasLimit, "Sub-Message OutOfGas panic")
			err = sdkerrors.Wrap(sdkerrors.ErrOutOfGas, "SubMsg hit gas limit")
		}
	}()
	events, data, err = k.messenger.DispatchMsg(subCtx, contractAddr, ibcPort, msg)

	// make sure we charge the parent what was spent
	spent := subCtx.GasMeter().GasConsumed()
	ctx.GasMeter().ConsumeGas(spent, "From limited Sub-Message")

	return events, data, err
}

// dispatchSubmessages builds a sandbox to execute these messages and returns the execution result to the contract
// that dispatched them, both on success as well as failure
func (k Keeper) dispatchSubmessages(ctx sdk.Context, contractAddr sdk.AccAddress, ibcPort string, msgs []wasmvmtypes.SubMsg) error {
	for _, msg := range msgs {
		// first, we build a sub-context which we can use inside the submessages
		subCtx, commit := ctx.CacheContext()

		// check how much gas left locally, optionally wrap the gas meter
		gasRemaining := ctx.GasMeter().Limit() - ctx.GasMeter().GasConsumed()
		limitGas := msg.GasLimit != nil && (*msg.GasLimit < gasRemaining)

		var err error
		var events []sdk.Event
		var data [][]byte
		if limitGas {
			events, data, err = k.dispatchMsgWithGasLimit(subCtx, contractAddr, ibcPort, msg.Msg, *msg.GasLimit)
		} else {
			events, data, err = k.messenger.DispatchMsg(subCtx, contractAddr, ibcPort, msg.Msg)
		}

		// if it succeeds, commit state changes from submessage, and pass on events to Event Manager
		if err == nil {
			commit()
			ctx.EventManager().EmitEvents(events)
		}
		// on failure, revert state from sandbox, and ignore events (just skip doing the above)

		var result wasmvmtypes.SubcallResult
		if err == nil {
			// just take the first one for now if there are multiple sub-sdk messages
			// and safely return nothing if no data
			var responseData []byte
			if len(data) > 0 {
				responseData = data[0]
			}
			result = wasmvmtypes.SubcallResult{
				Ok: &wasmvmtypes.SubcallResponse{
					Events: sdkEventsToWasmVMEvents(events),
					Data:   responseData,
				},
			}
		} else {
			result = wasmvmtypes.SubcallResult{
				Err: err.Error(),
			}
		}

		// now handle the reply, we use the parent context, and abort on error
		reply := wasmvmtypes.Reply{
			ID:     msg.ID,
			Result: result,
		}

		// we can ignore any result returned as there is nothing to do with the data
		// and the events are already in the ctx.EventManager()
		_, err = k.reply(ctx, contractAddr, reply)
		if err != nil {
			return err
		}
	}
	return nil
}

func sdkEventsToWasmVMEvents(events []sdk.Event) []wasmvmtypes.Event {
	res := make([]wasmvmtypes.Event, len(events))
	for i, ev := range events {
		res[i] = wasmvmtypes.Event{
			Type:       ev.Type,
			Attributes: sdkAttributesToWasmVMAttributes(ev.Attributes),
		}
	}
	return res
}

func sdkAttributesToWasmVMAttributes(attrs []abci.EventAttribute) []wasmvmtypes.EventAttribute {
	res := make([]wasmvmtypes.EventAttribute, len(attrs))
	for i, attr := range attrs {
		res[i] = wasmvmtypes.EventAttribute{
			Key:   string(attr.Key),
			Value: string(attr.Value),
		}
	}
	return res
}

func gasForContract(ctx sdk.Context, gasMultiplier uint64) uint64 {
	meter := ctx.GasMeter()
	if meter.IsOutOfGas() {
		return 0
	}
	remaining := (meter.Limit() - meter.GasConsumedToLimit()) * gasMultiplier
	if remaining > MaxGas {
		return MaxGas
	}
	return remaining
}

func (k Keeper) consumeGas(ctx sdk.Context, gas uint64) {
	consumed := gas / k.getGasMultiplier(ctx)
	ctx.GasMeter().ConsumeGas(consumed, "wasm contract")
	// throw OutOfGas error if we ran out (got exactly to zero due to better limit enforcing)
	if ctx.GasMeter().IsOutOfGas() {
		panic(sdk.ErrorOutOfGas{Descriptor: "Wasmer function execution"})
	}
}

// generates a contract address from codeID + instanceID
func (k Keeper) generateContractAddress(ctx sdk.Context, codeID uint64) sdk.AccAddress {
	instanceID := k.autoIncrementID(ctx, types.KeyLastInstanceID)
	return contractAddress(codeID, instanceID)
}

// contractAddress builds an sdk account address for a contract.
// Intentionally kept private as this is module internal logic.
func contractAddress(codeID, instanceID uint64) sdk.AccAddress {
	// NOTE: It is possible to get a duplicate address if either codeID or instanceID
	// overflow 32 bits. This is highly improbable, but something that could be refactored.
	contractID := codeID<<32 + instanceID
	return addrFromUint64(contractID)
}

// GetNextCodeID reads the next sequence id used for storing wasm code.
// Read only operation.
func (k Keeper) GetNextCodeID(ctx sdk.Context) uint64 {
	store := ctx.KVStore(k.storeKey)
	bz := store.Get(types.KeyLastCodeID)
	id := uint64(1)
	if bz != nil {
		id = binary.BigEndian.Uint64(bz)
	}
	return id
}

func (k Keeper) autoIncrementID(ctx sdk.Context, lastIDKey []byte) uint64 {
	store := ctx.KVStore(k.storeKey)
	bz := store.Get(lastIDKey)
	id := uint64(1)
	if bz != nil {
		id = binary.BigEndian.Uint64(bz)
	}
	bz = sdk.Uint64ToBigEndian(id + 1)
	store.Set(lastIDKey, bz)
	return id
}

// peekAutoIncrementID reads the current value without incrementing it.
func (k Keeper) peekAutoIncrementID(ctx sdk.Context, lastIDKey []byte) uint64 {
	store := ctx.KVStore(k.storeKey)
	bz := store.Get(lastIDKey)
	id := uint64(1)
	if bz != nil {
		id = binary.BigEndian.Uint64(bz)
	}
	return id
}

func (k Keeper) importAutoIncrementID(ctx sdk.Context, lastIDKey []byte, val uint64) error {
	store := ctx.KVStore(k.storeKey)
	if store.Has(lastIDKey) {
		return sdkerrors.Wrapf(types.ErrDuplicate, "autoincrement id: %s", string(lastIDKey))
	}
	bz := sdk.Uint64ToBigEndian(val)
	store.Set(lastIDKey, bz)
	return nil
}

func (k Keeper) importContract(ctx sdk.Context, contractAddr sdk.AccAddress, c *types.ContractInfo, state []types.Model) error {
	if !k.containsCodeInfo(ctx, c.CodeID) {
		return sdkerrors.Wrapf(types.ErrNotFound, "code id: %d", c.CodeID)
	}
	if k.containsContractInfo(ctx, contractAddr) {
		return sdkerrors.Wrapf(types.ErrDuplicate, "contract: %s", contractAddr)
	}

	historyEntry := c.ResetFromGenesis(ctx)
	k.appendToContractHistory(ctx, contractAddr, historyEntry)
	k.storeContractInfo(ctx, contractAddr, c)
	return k.importContractState(ctx, contractAddr, state)
}

func addrFromUint64(id uint64) sdk.AccAddress {
	addr := make([]byte, 20)
	addr[0] = 'C'
	binary.PutUvarint(addr[1:], id)
	return sdk.AccAddress(crypto.AddressHash(addr))
}

// MultipliedGasMeter wraps the GasMeter from context and multiplies all reads by out defined multiplier
type MultipliedGasMeter struct {
	originalMeter sdk.GasMeter
	gasMultiplier uint64
}

var _ wasmvm.GasMeter = MultipliedGasMeter{}

func (m MultipliedGasMeter) GasConsumed() sdk.Gas {
	return m.originalMeter.GasConsumed() * m.gasMultiplier
}

func (k Keeper) gasMeter(ctx sdk.Context) MultipliedGasMeter {
	return MultipliedGasMeter{
		originalMeter: ctx.GasMeter(),
		gasMultiplier: k.getGasMultiplier(ctx),
	}
}

// Logger returns a module-specific logger.
func (k Keeper) Logger(ctx sdk.Context) log.Logger {
	return ctx.Logger().With("module", fmt.Sprintf("x/%s", types.ModuleName))
}

// CoinTransferrer replicates the cosmos-sdk behaviour as in
// lfb-sdk's x/bank/keeper/msg_server.go Send
// (https://github.com/line/lfb-sdk/blob/2a5a2d2c885b03e278bcd67546d4f21e74614ead/x/bank/keeper/msg_server.go#L26)
type CoinTransferrer struct {
	keeper types.BankKeeper
}

func NewBankCoinTransferrer(keeper types.BankKeeper) CoinTransferrer {
	return CoinTransferrer{
		keeper: keeper,
	}
}

// TransferCoins transfers coins from source to destination account when coin send was enabled for them and the recipient
// is not in the blocked address list.
func (c CoinTransferrer) TransferCoins(ctx sdk.Context, fromAddr sdk.AccAddress, toAddr sdk.AccAddress, amt sdk.Coins) error {
	if err := c.keeper.SendEnabledCoins(ctx, amt...); err != nil {
		return err
	}
	if c.keeper.BlockedAddr(fromAddr) {
		return sdkerrors.Wrap(sdkerrors.ErrInvalidAddress, "blocked address can not be used")
	}
	sdkerr := c.keeper.SendCoins(ctx, fromAddr, toAddr, amt)
	if sdkerr != nil {
		return sdkerr
	}
	return nil
}
