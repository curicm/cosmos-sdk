package rootmulti

import (
	"fmt"
	"reflect"
	"strings"

	abci "github.com/tendermint/tendermint/abci/types"
	"github.com/tendermint/tendermint/crypto/merkle"
	"github.com/tendermint/tendermint/crypto/tmhash"
	dbm "github.com/tendermint/tendermint/libs/db"

	sdk "github.com/cosmos/cosmos-sdk/types"

	"github.com/cosmos/cosmos-sdk/store/cachemulti"
	"github.com/cosmos/cosmos-sdk/store/trace"
)

const (
	latestVersionKey = "s/latest"
	commitInfoKeyFmt = "s/%d" // s/<version>
)

// Store is composed of many sdk.CommitStores. Name contrasts with
// cacheMultiStore which is for cache-wrapping other sdk.MultiStores. It implements
// the CommitMultiStore interface.
type Store struct {
	db           dbm.DB
	lastCommitID sdk.CommitID
	pruning      sdk.PruningStrategy
	storesParams map[sdk.StoreKey]storeParams
	stores       map[sdk.StoreKey]sdk.CommitKVStore
	keysByName   map[string]sdk.StoreKey

	tracer *sdk.Tracer
}

var _ sdk.CommitMultiStore = (*Store)(nil)
var _ sdk.Queryable = (*Store)(nil)

// nolint
func NewCommitMultiStore(db dbm.DB) *Store {
	return &Store{
		db:           db,
		storesParams: make(map[sdk.StoreKey]storeParams),
		stores:       make(map[sdk.StoreKey]sdk.CommitKVStore),
		keysByName:   make(map[string]sdk.StoreKey),
	}
}

// Implements MultiStore
func (rs *Store) GetTracer() *sdk.Tracer {
	return rs.tracer
}

// Implements CommitMultiStore
func (rs *Store) SetPruning(pruning sdk.PruningStrategy) {
	rs.pruning = pruning
	for _, substore := range rs.stores {
		substore.SetPruning(pruning)
	}
}

// Implements CommitMultiStore.
func (rs *Store) MountStoreWithDB(key sdk.StoreKey, db dbm.DB) {
	if key == nil {
		panic("MountIAVLStore() key cannot be nil")
	}
	if _, ok := rs.storesParams[key]; ok {
		panic(fmt.Sprintf("Store duplicate store key %v", key))
	}
	if _, ok := rs.keysByName[key.Name()]; ok {
		panic(fmt.Sprintf("Store duplicate store key name %v", key))
	}
	rs.storesParams[key] = storeParams{
		key: key,
		db:  db,
	}
	rs.keysByName[key.Name()] = key
}

// Implements CommitMultiStore.
func (rs *Store) GetCommitStore(key sdk.StoreKey) sdk.CommitStore {
	return rs.stores[key]
}

// Implements CommitMultiStore.
func (rs *Store) GetCommitKVStore(key sdk.StoreKey) sdk.CommitKVStore {
	return rs.stores[key].(sdk.CommitKVStore)
}

// Implements CommitMultiStore.
func (rs *Store) LoadLatestVersion() error {
	ver := getLatestVersion(rs.db)
	return rs.LoadMultiStoreVersion(ver)
}

// Implements CommitMultiStore.
func (rs *Store) LoadMultiStoreVersion(ver int64) error {
	// Convert StoreInfos slice to map
	var lastCommitID sdk.CommitID
	infos := make(map[sdk.StoreKey]storeInfo)
	if ver != 0 {
		// Get commitInfo
		cInfo, err := getCommitInfo(rs.db, ver)
		if err != nil {
			return err
		}

		for _, storeInfo := range cInfo.StoreInfos {
			infos[rs.nameToKey(storeInfo.Name)] = storeInfo
		}

		lastCommitID = cInfo.CommitID()
	}

	// Load each Store
	var newStores = make(map[sdk.StoreKey]sdk.CommitKVStore)
	for key, storeParams := range rs.storesParams {
		var id sdk.CommitID
		if info, ok := infos[key]; ok {
			id = info.Core.CommitID
		}
		store, err := rs.loadCommitStoreFromParams(key, id, storeParams)
		if err != nil {
			return fmt.Errorf("failed to load Store: %v", err)
		}
		newStores[key] = store
	}

	// Success.
	rs.lastCommitID = lastCommitID
	rs.stores = newStores
	return nil
}

//----------------------------------------
// +CommitStore

// Implements Committer/CommitStore.
func (rs *Store) LastCommitID() sdk.CommitID {
	return rs.lastCommitID
}

// Implements Committer/CommitStore.
func (rs *Store) Commit() sdk.CommitID {

	// Commit stores.
	version := rs.lastCommitID.Version + 1
	commitInfo := commitStores(version, rs.stores)

	// Need to update atomically.
	batch := rs.db.NewBatch()
	setCommitInfo(batch, version, commitInfo)
	setLatestVersion(batch, version)
	batch.Write()

	// Prepare for next version.
	commitID := sdk.CommitID{
		Version: version,
		Hash:    commitInfo.Hash(),
	}
	rs.lastCommitID = commitID
	return commitID
}

//----------------------------------------
// +MultiStore

// Implements sdk.MultiStore.
func (rs *Store) CacheWrap() sdk.CacheMultiStore {
	return cachemulti.NewStore(rs.db, rs.keysByName, rs.stores, rs.tracer)
}

// GetKVStore implements the sdk.MultiStore interface. If tracing is enabled on the
// Store, a wrapped TraceKVStore will be returned with the given
// tracer, otherwise, the original sdk.KVStore will be returned.
func (rs *Store) GetKVStore(key sdk.StoreKey) sdk.KVStore {
	store := rs.stores[key].(sdk.KVStore)

	if rs.tracer.Enabled() {
		store = trace.NewStore(store, rs.tracer)
	}

	return store
}

// Implements sdk.MultiStore

// getStoreByName will first convert the original name to
// a special key, before looking up the sdk.CommitStore.
// This is not exposed to the extensions (which will need the
// sdk.StoreKey), but is useful in main, and particularly app.Query,
// in order to convert human strings into sdk.CommitStores.
func (rs *Store) getStoreByName(name string) sdk.KVStore {
	key := rs.keysByName[name]
	if key == nil {
		return nil
	}
	return rs.stores[key]
}

//---------------------- Query ------------------

// Query calls substore.Query with the same `req` where `req.Path` is
// modified to remove the substore prefix.
// Ie. `req.Path` here is `/<substore>/<path>`, and trimmed to `/<path>` for the substore.
// TODO: add proof for `multistore -> substore`.
func (rs *Store) Query(req abci.RequestQuery) abci.ResponseQuery {
	// Query just routes this to a substore.
	path := req.Path
	storeName, subpath, err := parsePath(path)
	if err != nil {
		return err.QueryResult()
	}

	store := rs.getStoreByName(storeName)
	if store == nil {
		msg := fmt.Sprintf("no such store: %s", storeName)
		return sdk.ErrUnknownRequest(msg).QueryResult()
	}
	queryable, ok := store.(sdk.Queryable)
	if !ok {
		msg := fmt.Sprintf("store %s doesn't support queries", storeName)
		return sdk.ErrUnknownRequest(msg).QueryResult()
	}

	// trim the path and make the query
	req.Path = subpath
	res := queryable.Query(req)

	if !req.Prove || !RequireProof(subpath) {
		return res
	}

	commitInfo, errMsg := getCommitInfo(rs.db, res.Height)
	if errMsg != nil {
		return sdk.ErrInternal(errMsg.Error()).QueryResult()
	}

	res.Proof = buildMultiStoreProof(res.Proof, storeName, commitInfo.StoreInfos)

	return res
}

// parsePath expects a format like /<storeName>[/<subpath>]
// Must start with /, subpath may be empty
// Returns error if it doesn't start with /
func parsePath(path string) (storeName string, subpath string, err sdk.Error) {
	if !strings.HasPrefix(path, "/") {
		err = sdk.ErrUnknownRequest(fmt.Sprintf("invalid path: %s", path))
		return
	}
	paths := strings.SplitN(path[1:], "/", 2)
	storeName = paths[0]
	if len(paths) == 2 {
		subpath = "/" + paths[1]
	}
	return
}

//----------------------------------------

func (rs *Store) loadCommitStoreFromParams(key sdk.StoreKey, id sdk.CommitID, params storeParams) (store sdk.CommitKVStore, err error) {
	var db dbm.DB
	if params.db != nil {
		db = dbm.NewPrefixDB(params.db, []byte("s/_/"))
	} else {
		db = dbm.NewPrefixDB(rs.db, []byte("s/k:"+params.key.Name()+"/"))
	}

	store = reflect.Zero(params.typ).Interface().(sdk.CommitKVStore)
	err = store.LoadKVStoreVersion(db, id)
	if err != nil {
		store.SetPruning(rs.pruning)
	}

	return

	// XXX: move to store subdirectories LoadKVStoreVersion
	/*
		switch params.typ {
		case sdk.StoreTypeMulti:
			panic("recursive sdk.MultiStores not yet supported")
			// TODO: id?
			// return NewCommitMultiStore(db, id)
		case sdk.StoreTypeIAVL:
			store, err = LoadIAVLStore(db, id, rs.pruning)
			return
		case sdk.StoreTypeDB:
			panic("dbm.DB is not a sdk.CommitStore")
		case sdk.StoreTypeTransient:
			_, ok := key.(*sdk.TransientStoreKey)
			if !ok {
				err = fmt.Errorf("invalid sdk.StoreKey for sdk.StoreTypeTransient: %s", key.String())
				return
			}
			store = transient.NewStore()
			return
		default:
			panic(fmt.Sprintf("unrecognized store type %v", params.typ))
		}
	*/
}

func (rs *Store) nameToKey(name string) sdk.StoreKey {
	for key := range rs.storesParams {
		if key.Name() == name {
			return key
		}
	}
	panic("Unknown name " + name)
}

//----------------------------------------
// storeParams

type storeParams struct {
	key sdk.StoreKey
	db  dbm.DB
	typ reflect.Type
}

//----------------------------------------
// commitInfo

// NOTE: Keep commitInfo a simple immutable struct.
type commitInfo struct {

	// Version
	Version int64

	// sdk.Store info for
	StoreInfos []storeInfo
}

// Hash returns the simple merkle root hash of the stores sorted by name.
func (ci commitInfo) Hash() []byte {
	// TODO cache to ci.hash []byte
	m := make(map[string]merkle.Hasher, len(ci.StoreInfos))
	for _, storeInfo := range ci.StoreInfos {
		m[storeInfo.Name] = storeInfo
	}
	return merkle.SimpleHashFromMap(m)
}

func (ci commitInfo) CommitID() sdk.CommitID {
	return sdk.CommitID{
		Version: ci.Version,
		Hash:    ci.Hash(),
	}
}

//----------------------------------------
// storeInfo

// storeInfo contains the name and core reference for an
// underlying store.  It is the leaf of the Stores top
// level simple merkle tree.
type storeInfo struct {
	Name string
	Core storeCore
}

type storeCore struct {
	// sdk.StoreType sdk.StoreType
	CommitID sdk.CommitID
	// ... maybe add more state
}

// Implements merkle.Hasher.
func (si storeInfo) Hash() []byte {
	// Doesn't write Name, since merkle.SimpleHashFromMap() will
	// include them via the keys.
	bz, _ := cdc.MarshalBinary(si.Core) // Does not error
	hasher := tmhash.New()
	_, err := hasher.Write(bz)
	if err != nil {
		// TODO: Handle with #870
		panic(err)
	}
	return hasher.Sum(nil)
}

//----------------------------------------
// Misc.

func getLatestVersion(db dbm.DB) int64 {
	var latest int64
	latestBytes := db.Get([]byte(latestVersionKey))
	if latestBytes == nil {
		return 0
	}
	err := cdc.UnmarshalBinary(latestBytes, &latest)
	if err != nil {
		panic(err)
	}
	return latest
}

// Set the latest version.
func setLatestVersion(batch dbm.Batch, version int64) {
	latestBytes, _ := cdc.MarshalBinary(version) // Does not error
	batch.Set([]byte(latestVersionKey), latestBytes)
}

// Commits each store and returns a new commitInfo.
func commitStores(version int64, storeMap map[sdk.StoreKey]sdk.CommitKVStore) commitInfo {
	storeInfos := make([]storeInfo, 0, len(storeMap))

	for key, store := range storeMap {
		// Commit
		commitID := store.Commit()

		if commitID.IsZero() {
			continue
		}

		// Record sdk.CommitID
		si := storeInfo{}
		si.Name = key.Name()
		si.Core.CommitID = commitID
		// si.Core.StoreType = store.GetStoreType()
		storeInfos = append(storeInfos, si)
	}

	ci := commitInfo{
		Version:    version,
		StoreInfos: storeInfos,
	}
	return ci
}

// Gets commitInfo from disk.
func getCommitInfo(db dbm.DB, ver int64) (commitInfo, error) {

	// Get from DB.
	cInfoKey := fmt.Sprintf(commitInfoKeyFmt, ver)
	cInfoBytes := db.Get([]byte(cInfoKey))
	if cInfoBytes == nil {
		return commitInfo{}, fmt.Errorf("failed to get Store: no data")
	}

	// Parse bytes.
	var cInfo commitInfo
	err := cdc.UnmarshalBinary(cInfoBytes, &cInfo)
	if err != nil {
		return commitInfo{}, fmt.Errorf("failed to get Store: %v", err)
	}
	return cInfo, nil
}

// Set a commitInfo for given version.
func setCommitInfo(batch dbm.Batch, version int64, cInfo commitInfo) {
	cInfoBytes, err := cdc.MarshalBinary(cInfo)
	if err != nil {
		panic(err)
	}
	cInfoKey := fmt.Sprintf(commitInfoKeyFmt, version)
	batch.Set([]byte(cInfoKey), cInfoBytes)
}
