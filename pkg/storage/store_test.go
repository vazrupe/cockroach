// Copyright 2014 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package storage

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"math/rand"
	"reflect"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/config"
	"github.com/cockroachdb/cockroach/pkg/gossip"
	"github.com/cockroachdb/cockroach/pkg/internal/client"
	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/rpc"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlbase"
	"github.com/cockroachdb/cockroach/pkg/storage/abortspan"
	"github.com/cockroachdb/cockroach/pkg/storage/batcheval"
	"github.com/cockroachdb/cockroach/pkg/storage/engine"
	"github.com/cockroachdb/cockroach/pkg/storage/engine/enginepb"
	"github.com/cockroachdb/cockroach/pkg/storage/rditer"
	"github.com/cockroachdb/cockroach/pkg/storage/stateloader"
	"github.com/cockroachdb/cockroach/pkg/storage/storagebase"
	"github.com/cockroachdb/cockroach/pkg/storage/storagepb"
	"github.com/cockroachdb/cockroach/pkg/storage/txnwait"
	"github.com/cockroachdb/cockroach/pkg/testutils"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/metric"
	"github.com/cockroachdb/cockroach/pkg/util/protoutil"
	"github.com/cockroachdb/cockroach/pkg/util/randutil"
	"github.com/cockroachdb/cockroach/pkg/util/stop"
	"github.com/cockroachdb/cockroach/pkg/util/uuid"
	"github.com/gogo/protobuf/proto"
	"github.com/kr/pretty"
	"github.com/pkg/errors"
	"go.etcd.io/etcd/raft"
	"go.etcd.io/etcd/raft/raftpb"
	"golang.org/x/time/rate"
)

var testIdent = roachpb.StoreIdent{
	ClusterID: uuid.MakeV4(),
	NodeID:    1,
	StoreID:   1,
}

func (s *Store) TestSender() client.Sender {
	return client.Wrap(s, func(ba roachpb.BatchRequest) roachpb.BatchRequest {
		if ba.RangeID != 0 {
			return ba
		}

		// If the client hasn't set ba.Range, we do it a favor and figure out the
		// range to which the request needs to go.
		//
		// NOTE: We don't use keys.Range(ba.Requests) here because that does some
		// validation on the batch, and some tests using this sender don't like
		// that.
		key, err := keys.Addr(ba.Requests[0].GetInner().Header().Key)
		if err != nil {
			log.Fatal(context.TODO(), err)
		}

		ba.RangeID = roachpb.RangeID(1)
		if repl := s.LookupReplica(key); repl != nil {
			ba.RangeID = repl.RangeID

			// Attempt to assign a Replica descriptor to the batch if
			// necessary, but don't throw an error if this fails.
			if ba.Replica == (roachpb.ReplicaDescriptor{}) {
				if desc, err := repl.GetReplicaDescriptor(); err == nil {
					ba.Replica = desc
				}
			}
		}
		return ba
	})
}

// testSenderFactory is an implementation of the
// client.TxnSenderFactory interface.
type testSenderFactory struct {
	store        *Store
	nonTxnSender *testSender
}

func (f *testSenderFactory) TransactionalSender(
	typ client.TxnType, coordMeta roachpb.TxnCoordMeta,
) client.TxnSender {
	return client.NewMockTransactionalSender(
		func(
			ctx context.Context, _ *roachpb.Transaction, ba roachpb.BatchRequest,
		) (*roachpb.BatchResponse, *roachpb.Error) {
			return f.store.Send(ctx, ba)
		},
		&coordMeta.Txn)
}

func (f *testSenderFactory) NonTransactionalSender() client.Sender {
	if f.nonTxnSender != nil {
		return f.nonTxnSender
	}
	f.nonTxnSender = &testSender{store: f.store}
	return f.nonTxnSender
}

func (f *testSenderFactory) setStore(s *Store) {
	f.store = s
	if f.nonTxnSender != nil {
		// monkey-patch an already created Sender, helping with test bootstrapping.
		f.nonTxnSender.store = s
	}
}

// testSender is an implementation of the client.TxnSender interface
// which passes all requests through to a single store.
type testSender struct {
	store *Store
}

// Send forwards the call to the single store. This is a poor man's
// version of kv.TxnCoordSender, but it serves the purposes of
// supporting tests in this package. Transactions are not supported.
// Since kv/ depends on storage/, we can't get access to a
// TxnCoordSender from here.
// TODO(tschottdorf): {kv->storage}.LocalSender
func (db *testSender) Send(
	ctx context.Context, ba roachpb.BatchRequest,
) (*roachpb.BatchResponse, *roachpb.Error) {
	if et, ok := ba.GetArg(roachpb.EndTransaction); ok {
		return nil, roachpb.NewErrorf("%s method not supported", et.Method())
	}
	// Lookup range and direct request.
	rs, err := keys.Range(ba.Requests)
	if err != nil {
		return nil, roachpb.NewError(err)
	}
	repl := db.store.LookupReplica(rs.Key)
	if repl == nil || !repl.Desc().ContainsKeyRange(rs.Key, rs.EndKey) {
		return nil, roachpb.NewError(roachpb.NewRangeKeyMismatchError(rs.Key.AsRawKey(), rs.EndKey.AsRawKey(), nil))
	}
	ba.RangeID = repl.RangeID
	repDesc, err := repl.GetReplicaDescriptor()
	if err != nil {
		return nil, roachpb.NewError(err)
	}
	ba.Replica = repDesc
	br, pErr := db.store.Send(ctx, ba)
	if br != nil && br.Error != nil {
		panic(roachpb.ErrorUnexpectedlySet(db.store, br))
	}
	if pErr != nil {
		return nil, pErr
	}
	return br, nil
}

// testStoreOpts affords control over aspects of store creation.
type testStoreOpts struct {
	// If createSystemRanges is not set, the store will have a single range. If
	// set, the store will have all the system ranges that are generally created
	// for a cluster at boostrap.
	createSystemRanges bool
}

// createTestStoreWithoutStart creates a test store using an in-memory
// engine without starting the store. It returns the store, the store
// clock's manual unix nanos time and a stopper. The caller is
// responsible for stopping the stopper upon completion.
// Some fields of ctx are populated by this function.
func createTestStoreWithoutStart(
	t testing.TB, stopper *stop.Stopper, opts testStoreOpts, cfg *StoreConfig,
) *Store {
	// Setup fake zone config handler.
	config.TestingSetupZoneConfigHook(stopper)

	rpcContext := rpc.NewContext(
		cfg.AmbientCtx, &base.Config{Insecure: true}, cfg.Clock,
		stopper, &cfg.Settings.Version)
	server := rpc.NewServer(rpcContext) // never started
	cfg.Gossip = gossip.NewTest(1, rpcContext, server, stopper, metric.NewRegistry(), cfg.DefaultZoneConfig)
	cfg.StorePool = NewTestStorePool(*cfg)
	// Many tests using this test harness (as opposed to higher-level
	// ones like multiTestContext or TestServer) want to micro-manage
	// replicas and the background queues just get in the way. The
	// scanner doesn't run frequently enough to expose races reliably,
	// so just disable the scanner for all tests that use this function
	// instead of figuring out exactly which tests need it.
	cfg.TestingKnobs.DisableScanner = true
	// The scanner affects background operations; we must also disable the split
	// and merge queues separately to cover event-driven splits and merges.
	cfg.TestingKnobs.DisableSplitQueue = true
	cfg.TestingKnobs.DisableMergeQueue = true
	eng := engine.NewInMem(roachpb.Attributes{}, 10<<20)
	stopper.AddCloser(eng)
	cfg.Transport = NewDummyRaftTransport(cfg.Settings)
	factory := &testSenderFactory{}
	cfg.DB = client.NewDB(cfg.AmbientCtx, factory, cfg.Clock)
	store := NewStore(context.TODO(), *cfg, eng, &roachpb.NodeDescriptor{NodeID: 1})
	factory.setStore(store)
	if err := InitEngine(
		context.TODO(), eng, roachpb.StoreIdent{NodeID: 1, StoreID: 1}, cfg.Settings.Version.BootstrapVersion(),
	); err != nil {
		t.Fatal(err)
	}
	var splits []roachpb.RKey
	kvs, tableSplits := sqlbase.MakeMetadataSchema(cfg.DefaultZoneConfig, cfg.DefaultSystemZoneConfig).GetInitialValues()
	if opts.createSystemRanges {
		splits = config.StaticSplits()
		splits = append(splits, tableSplits...)
		sort.Slice(splits, func(i, j int) bool {
			return splits[i].Less(splits[j])
		})
	}
	if err := WriteInitialClusterData(
		context.TODO(), eng, kvs /* initialValues */, cfg.Settings.Version.BootstrapVersion().Version,
		1 /* numStores */, splits, cfg.Clock.PhysicalNow(),
	); err != nil {
		t.Fatal(err)
	}
	return store
}

func createTestStore(
	t testing.TB, opts testStoreOpts, stopper *stop.Stopper,
) (*Store, *hlc.ManualClock) {
	manual := hlc.NewManualClock(123)
	cfg := TestStoreConfig(hlc.NewClock(manual.UnixNano, time.Nanosecond))
	store := createTestStoreWithConfig(t, stopper, opts, &cfg)
	return store, manual
}

// createTestStore creates a test store using an in-memory
// engine. It returns the store, the store clock's manual unix nanos time
// and a stopper. The caller is responsible for stopping the stopper
// upon completion.
func createTestStoreWithConfig(
	t testing.TB, stopper *stop.Stopper, opts testStoreOpts, cfg *StoreConfig,
) *Store {
	store := createTestStoreWithoutStart(t, stopper, opts, cfg)
	// Put an empty system config into gossip.
	if err := store.Gossip().AddInfoProto(gossip.KeySystemConfig,
		&config.SystemConfigEntries{}, 0); err != nil {
		t.Fatal(err)
	}
	if err := store.Start(context.Background(), stopper); err != nil {
		t.Fatal(err)
	}
	store.WaitForInit()
	return store
}

// TestIterateIDPrefixKeys lays down a number of tombstones (at keys.RaftTombstoneKey) interspersed
// with other irrelevant keys (both chosen randomly). It then verifies that IterateIDPrefixKeys
// correctly returns only the relevant keys and values.
func TestIterateIDPrefixKeys(t *testing.T) {
	defer leaktest.AfterTest(t)()

	ctx := context.Background()
	stopper := stop.NewStopper()
	defer stopper.Stop(ctx)

	eng := engine.NewInMem(roachpb.Attributes{}, 1<<20)
	stopper.AddCloser(eng)

	seed := randutil.NewPseudoSeed()
	//const seed = -1666367124291055473
	t.Logf("seed is %d", seed)
	rng := rand.New(rand.NewSource(seed))

	ops := []func(rangeID roachpb.RangeID) roachpb.Key{
		keys.RaftAppliedIndexLegacyKey, // replicated; sorts before tombstone
		keys.RaftHardStateKey,          // unreplicated; sorts after tombstone
		// Replicated key-anchored local key (i.e. not one we should care about).
		// Will be written at zero timestamp, but that's ok.
		func(rangeID roachpb.RangeID) roachpb.Key {
			return keys.RangeDescriptorKey([]byte(fmt.Sprintf("fakerange%d", rangeID)))
		},
		func(rangeID roachpb.RangeID) roachpb.Key {
			return roachpb.Key(fmt.Sprintf("fakeuserkey%d", rangeID))
		},
	}

	const rangeCount = 10
	rangeIDFn := func() roachpb.RangeID {
		return 1 + roachpb.RangeID(rng.Intn(10*rangeCount)) // spread rangeIDs out
	}

	// Write a number of keys that should be irrelevant to the iteration in this test.
	for i := 0; i < rangeCount; i++ {
		rangeID := rangeIDFn()

		// Grab between one and all ops, randomly.
		for _, opIdx := range rng.Perm(len(ops))[:rng.Intn(1+len(ops))] {
			key := ops[opIdx](rangeID)
			t.Logf("writing op=%d rangeID=%d", opIdx, rangeID)
			if err := engine.MVCCPut(
				ctx,
				eng,
				nil, /* ms */
				key,
				hlc.Timestamp{},
				roachpb.MakeValueFromString("fake value for "+key.String()),
				nil, /* txn */
			); err != nil {
				t.Fatal(err)
			}
		}
	}

	type seenT struct {
		rangeID   roachpb.RangeID
		tombstone roachpb.RaftTombstone
	}

	// Next, write the keys we're planning to see again.
	var wanted []seenT
	{
		used := make(map[roachpb.RangeID]struct{})
		for {
			rangeID := rangeIDFn()
			if _, ok := used[rangeID]; ok {
				// We already wrote this key, so roll the dice again.
				continue
			}

			tombstone := roachpb.RaftTombstone{
				NextReplicaID: roachpb.ReplicaID(rng.Int31n(100)),
			}

			used[rangeID] = struct{}{}
			wanted = append(wanted, seenT{rangeID: rangeID, tombstone: tombstone})

			t.Logf("writing tombstone at rangeID=%d", rangeID)
			if err := engine.MVCCPutProto(
				ctx, eng, nil /* ms */, keys.RaftTombstoneKey(rangeID), hlc.Timestamp{}, nil /* txn */, &tombstone,
			); err != nil {
				t.Fatal(err)
			}

			if len(wanted) >= rangeCount {
				break
			}
		}
	}

	sort.Slice(wanted, func(i, j int) bool {
		return wanted[i].rangeID < wanted[j].rangeID
	})

	var seen []seenT
	var tombstone roachpb.RaftTombstone

	handleTombstone := func(rangeID roachpb.RangeID) (more bool, _ error) {
		seen = append(seen, seenT{rangeID: rangeID, tombstone: tombstone})
		return true, nil
	}

	if err := IterateIDPrefixKeys(ctx, eng, keys.RaftTombstoneKey, &tombstone, handleTombstone); err != nil {
		t.Fatal(err)
	}
	placeholder := seenT{
		rangeID: roachpb.RangeID(9999),
	}

	if len(wanted) != len(seen) {
		t.Errorf("wanted %d results, got %d", len(wanted), len(seen))
	}

	for len(wanted) < len(seen) {
		wanted = append(wanted, placeholder)
	}
	for len(seen) < len(wanted) {
		seen = append(seen, placeholder)
	}

	if diff := pretty.Diff(wanted, seen); len(diff) > 0 {
		pretty.Ldiff(t, wanted, seen)
		t.Fatal("diff(wanted, seen) is nonempty")
	}
}

// TestStoreInitAndBootstrap verifies store initialization and bootstrap.
func TestStoreInitAndBootstrap(t *testing.T) {
	defer leaktest.AfterTest(t)()

	// We need a fixed clock to avoid LastUpdateNanos drifting on us.
	cfg := TestStoreConfig(hlc.NewClock(func() int64 { return 123 }, time.Nanosecond))
	stopper := stop.NewStopper()
	ctx := context.TODO()
	defer stopper.Stop(ctx)
	eng := engine.NewInMem(roachpb.Attributes{}, 1<<20)
	stopper.AddCloser(eng)
	cfg.Transport = NewDummyRaftTransport(cfg.Settings)
	factory := &testSenderFactory{}
	cfg.DB = client.NewDB(cfg.AmbientCtx, factory, cfg.Clock)
	{
		store := NewStore(ctx, cfg, eng, &roachpb.NodeDescriptor{NodeID: 1})
		// Can't start as haven't bootstrapped.
		if err := store.Start(ctx, stopper); err == nil {
			t.Error("expected failure starting un-bootstrapped store")
		}

		// Bootstrap with a fake ident.
		if err := InitEngine(ctx, eng, testIdent, cfg.Settings.Version.BootstrapVersion()); err != nil {
			t.Errorf("error bootstrapping store: %+v", err)
		}

		// Verify we can read the store ident after a flush.
		if err := eng.Flush(); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadStoreIdent(ctx, eng); err != nil {
			t.Fatalf("unable to read store ident: %+v", err)
		}

		// Bootstrap the system ranges.
		var splits []roachpb.RKey
		kvs, tableSplits := sqlbase.MakeMetadataSchema(cfg.DefaultZoneConfig, cfg.DefaultSystemZoneConfig).GetInitialValues()
		splits = config.StaticSplits()
		splits = append(splits, tableSplits...)
		sort.Slice(splits, func(i, j int) bool {
			return splits[i].Less(splits[j])
		})

		if err := WriteInitialClusterData(
			ctx, eng, kvs /* initialValues */, cfg.Settings.Version.BootstrapVersion().Version,
			1 /* numStores */, splits, cfg.Clock.PhysicalNow(),
		); err != nil {
			t.Errorf("failure to create first range: %+v", err)
		}
	}

	// Now, attempt to initialize a store with a now-bootstrapped range.
	store := NewStore(ctx, cfg, eng, &roachpb.NodeDescriptor{NodeID: 1})
	if err := store.Start(ctx, stopper); err != nil {
		t.Fatalf("failure initializing bootstrapped store: %+v", err)
	}

	for i := 1; i <= store.ReplicaCount(); i++ {
		r, err := store.GetReplica(roachpb.RangeID(i))
		if err != nil {
			t.Fatalf("failure fetching range %d: %+v", i, err)
		}
		rs := r.GetMVCCStats()

		// Stats should agree with a recomputation.
		now := r.store.Clock().Now()
		if ms, err := rditer.ComputeStatsForRange(r.Desc(), eng, now.WallTime); err != nil {
			t.Errorf("failure computing range's stats: %+v", err)
		} else if ms != rs {
			t.Errorf("expected range's stats to agree with recomputation: %s", pretty.Diff(ms, rs))
		}
	}
}

// TestBootstrapOfNonEmptyStore verifies bootstrap failure if engine
// is not empty.
func TestBootstrapOfNonEmptyStore(t *testing.T) {
	defer leaktest.AfterTest(t)()
	stopper := stop.NewStopper()
	ctx := context.TODO()
	defer stopper.Stop(ctx)
	eng := engine.NewInMem(roachpb.Attributes{}, 1<<20)
	stopper.AddCloser(eng)

	// Put some random garbage into the engine.
	if err := eng.Put(engine.MakeMVCCMetadataKey(roachpb.Key("foo")), []byte("bar")); err != nil {
		t.Errorf("failure putting key foo into engine: %+v", err)
	}
	cfg := TestStoreConfig(nil)
	cfg.Transport = NewDummyRaftTransport(cfg.Settings)
	store := NewStore(ctx, cfg, eng, &roachpb.NodeDescriptor{NodeID: 1})

	// Can't init as haven't bootstrapped.
	switch err := errors.Cause(store.Start(ctx, stopper)); err.(type) {
	case *NotBootstrappedError:
	default:
		t.Errorf("unexpected error initializing un-bootstrapped store: %+v", err)
	}

	// Bootstrap should fail on non-empty engine.
	switch err := errors.Cause(InitEngine(ctx, eng, testIdent, cfg.Settings.Version.BootstrapVersion())); err.(type) {
	case *NotBootstrappedError:
	default:
		t.Errorf("unexpected error bootstrapping non-empty store: %+v", err)
	}
}

// create a Replica and add it to the store. Note that replicas
// created in this way do not have their raft groups fully initialized
// so most KV operations will not work on them. This function is
// deprecated; new tests should create replicas by splitting from a
// properly-bootstrapped initial range.
func createReplica(s *Store, rangeID roachpb.RangeID, start, end roachpb.RKey) *Replica {
	desc := &roachpb.RangeDescriptor{
		RangeID:  rangeID,
		StartKey: start,
		EndKey:   end,
		InternalReplicas: []roachpb.ReplicaDescriptor{{
			NodeID:    1,
			StoreID:   1,
			ReplicaID: 1,
		}},
		NextReplicaID: 2,
	}
	r, err := NewReplica(desc, s, 0)
	if err != nil {
		log.Fatal(context.Background(), err)
	}
	return r
}

func TestStoreAddRemoveRanges(t *testing.T) {
	defer leaktest.AfterTest(t)()
	stopper := stop.NewStopper()
	defer stopper.Stop(context.TODO())
	store, _ := createTestStore(t,
		testStoreOpts{
			// This test was written before test stores could start with more than one
			// range and was not adapted.
			createSystemRanges: false,
		},
		stopper)
	if _, err := store.GetReplica(0); err == nil {
		t.Error("expected GetRange to fail on missing range")
	}
	// Range 1 already exists. Make sure we can fetch it.
	repl1, err := store.GetReplica(1)
	if err != nil {
		t.Error(err)
	}
	// Remove range 1.
	if err := store.RemoveReplica(context.Background(), repl1, repl1.Desc().NextReplicaID, RemoveOptions{
		DestroyData: true,
	}); err != nil {
		t.Error(err)
	}
	// Create a new range (id=2).
	repl2 := createReplica(store, 2, roachpb.RKey("a"), roachpb.RKey("b"))
	if err := store.AddReplica(repl2); err != nil {
		t.Fatal(err)
	}
	// Try to add the same range twice
	err = store.AddReplica(repl2)
	if err == nil {
		t.Fatal("expected error re-adding same range")
	}
	// Try to remove range 1 again.
	if err := store.RemoveReplica(context.Background(), repl1, repl1.Desc().NextReplicaID, RemoveOptions{
		DestroyData: true,
	}); err == nil {
		t.Fatal("expected error re-removing same range")
	}
	// Try to add a range with previously-used (but now removed) ID.
	repl2Dup := createReplica(store, 1, roachpb.RKey("a"), roachpb.RKey("b"))
	if err := store.AddReplica(repl2Dup); err == nil {
		t.Fatal("expected error inserting a duplicated range")
	}
	// Add another range with different key range and then test lookup.
	repl3 := createReplica(store, 3, roachpb.RKey("c"), roachpb.RKey("d"))
	if err := store.AddReplica(repl3); err != nil {
		t.Fatal(err)
	}

	testCases := []struct {
		key    roachpb.RKey
		expRng *Replica
	}{
		{roachpb.RKey("a"), repl2},
		{roachpb.RKey("a\xff\xff"), repl2},
		{roachpb.RKey("c"), repl3},
		{roachpb.RKey("c\xff\xff"), repl3},
		{roachpb.RKey("x60\xff\xff"), nil},
		{roachpb.RKey("x60\xff\xff"), nil},
		{roachpb.RKey("d"), nil},
	}

	for i, test := range testCases {
		if r := store.LookupReplica(test.key); r != test.expRng {
			t.Errorf("%d: expected range %v; got %v", i, test.expRng, r)
		}
	}
}

// TestReplicasByKey tests that operations that depend on the
// store.replicasByKey map function correctly when the underlying replicas'
// start and end keys are manipulated in place. This mutation happens when a
// snapshot is applied that advances a replica past a split.
func TestReplicasByKey(t *testing.T) {
	defer leaktest.AfterTest(t)()
	stopper := stop.NewStopper()
	defer stopper.Stop(context.TODO())
	store, _ := createTestStore(t,
		testStoreOpts{
			// This test was written before test stores could start with more than one
			// range and was not adapted.
			createSystemRanges: false,
		},
		stopper)

	// Shrink the main replica.
	rep, err := store.GetReplica(1)
	if err != nil {
		t.Fatal(err)
	}

	rep.mu.Lock()
	desc := *rep.mu.state.Desc // shallow copy to replace desc wholesale
	desc.EndKey = roachpb.RKey("e")
	rep.mu.state.Desc = &desc
	rep.mu.Unlock()

	// Ensure that this shrinkage is recognized by future additions to replicasByKey.
	reps := []*struct {
		replica            *Replica
		id                 int
		start, end         roachpb.RKey
		expectedErrorOnAdd string
	}{
		// [a,c) is contained in [KeyMin, e)
		{nil, 2, roachpb.RKey("a"), roachpb.RKey("c"), ".*has overlapping range"},
		// [c,f) partially overlaps with [KeyMin, e)
		{nil, 3, roachpb.RKey("c"), roachpb.RKey("f"), ".*has overlapping range"},
		// [e, f) is disjoint from [KeyMin, e)
		{nil, 4, roachpb.RKey("e"), roachpb.RKey("f"), ""},
	}

	for i, desc := range reps {
		desc.replica = createReplica(store, roachpb.RangeID(desc.id), desc.start, desc.end)
		err := store.AddReplica(desc.replica)
		if !testutils.IsError(err, desc.expectedErrorOnAdd) {
			t.Fatalf("adding replica %d: expected err %q, but encountered %v", i, desc.expectedErrorOnAdd, err)
		}
	}
}

func TestStoreRemoveReplicaOldDescriptor(t *testing.T) {
	defer leaktest.AfterTest(t)()
	ctx := context.Background()
	stopper := stop.NewStopper()
	defer stopper.Stop(ctx)
	store, _ := createTestStore(t, testStoreOpts{createSystemRanges: true}, stopper)

	rep, err := store.GetReplica(1)
	if err != nil {
		t.Fatal(err)
	}

	// First try and fail with a stale descriptor.
	origDesc := rep.Desc()
	newDesc := protoutil.Clone(origDesc).(*roachpb.RangeDescriptor)
	for i := range newDesc.InternalReplicas {
		if newDesc.InternalReplicas[i].StoreID == store.StoreID() {
			newDesc.InternalReplicas[i].ReplicaID++
			newDesc.NextReplicaID++
			break
		}
	}

	rep.setDesc(ctx, newDesc)
	expectedErr := "replica descriptor's ID has changed"
	if err := store.RemoveReplica(ctx, rep, origDesc.NextReplicaID, RemoveOptions{
		DestroyData: true,
	}); !testutils.IsError(err, expectedErr) {
		t.Fatalf("expected error %q but got %v", expectedErr, err)
	}

	// Now try a fresh descriptor and succeed.
	if err := store.RemoveReplica(ctx, rep, rep.Desc().NextReplicaID, RemoveOptions{
		DestroyData: true,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestStoreRemoveReplicaDestroy(t *testing.T) {
	defer leaktest.AfterTest(t)()
	stopper := stop.NewStopper()
	defer stopper.Stop(context.TODO())
	store, _ := createTestStore(t, testStoreOpts{createSystemRanges: true}, stopper)

	repl1, err := store.GetReplica(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RemoveReplica(context.Background(), repl1, repl1.Desc().NextReplicaID, RemoveOptions{
		DestroyData: true,
	}); err != nil {
		t.Fatal(err)
	}

	// Verify that removal of a replica marks it as destroyed so that future raft
	// commands on the Replica will silently be dropped.
	if err := repl1.withRaftGroup(true, func(r *raft.RawNode) (bool, error) {
		return true, errors.Errorf("unexpectedly created a raft group")
	}); err != nil {
		t.Fatal(err)
	}

	repl1.mu.Lock()
	expErr := roachpb.NewError(repl1.mu.destroyStatus.err)
	lease := *repl1.mu.state.Lease
	repl1.mu.Unlock()

	if expErr == nil {
		t.Fatal("replica was not marked as destroyed")
	}

	if _, _, _, pErr := repl1.evalAndPropose(
		context.Background(), lease, &roachpb.BatchRequest{}, &allSpans, endCmds{},
	); !pErr.Equal(expErr) {
		t.Fatalf("expected error %s, but got %v", expErr, pErr)
	}
}

func TestStoreReplicaVisitor(t *testing.T) {
	defer leaktest.AfterTest(t)()
	stopper := stop.NewStopper()
	defer stopper.Stop(context.TODO())
	store, _ := createTestStore(t,
		testStoreOpts{
			// This test was written before test stores could start with more than one
			// range and was not adapted.
			createSystemRanges: false,
		},
		stopper)

	// Remove range 1.
	repl1, err := store.GetReplica(1)
	if err != nil {
		t.Error(err)
	}
	if err := store.RemoveReplica(context.Background(), repl1, repl1.Desc().NextReplicaID, RemoveOptions{
		DestroyData: true,
	}); err != nil {
		t.Error(err)
	}

	// Add 10 new ranges.
	const newCount = 10
	for i := 0; i < newCount; i++ {
		repl := createReplica(store, roachpb.RangeID(i+1), roachpb.RKey(fmt.Sprintf("a%02d", i)), roachpb.RKey(fmt.Sprintf("a%02d", i+1)))
		if err := store.AddReplica(repl); err != nil {
			t.Fatal(err)
		}
	}

	// Verify two passes of the visit, the second one in-order.
	visitor := newStoreReplicaVisitor(store)
	exp := make(map[roachpb.RangeID]struct{})
	for i := 0; i < newCount; i++ {
		exp[roachpb.RangeID(i+1)] = struct{}{}
	}

	for pass := 0; pass < 2; pass++ {
		if ec := visitor.EstimatedCount(); ec != 10 {
			t.Fatalf("expected 10 remaining; got %d", ec)
		}
		i := 1
		seen := make(map[roachpb.RangeID]struct{})

		// Ensure that our next pass is done in-order.
		if pass == 1 {
			_ = visitor.InOrder()
		}
		var lastRangeID roachpb.RangeID
		visitor.Visit(func(repl *Replica) bool {
			if pass == 1 {
				if repl.RangeID <= lastRangeID {
					t.Fatalf("on second pass, expect ranges to be visited in ascending range ID order; %d !> %d", repl.RangeID, lastRangeID)
				}
				lastRangeID = repl.RangeID
			}
			_, ok := seen[repl.RangeID]
			if ok {
				t.Fatalf("already saw %d", repl.RangeID)
			}

			seen[repl.RangeID] = struct{}{}
			if ec := visitor.EstimatedCount(); ec != 10-i {
				t.Fatalf(
					"expected %d remaining; got %d after seeing %+v",
					10-i, ec, seen,
				)
			}
			i++
			return true
		})
		if ec := visitor.EstimatedCount(); ec != 10 {
			t.Fatalf("expected 10 remaining; got %d", ec)
		}
		if !reflect.DeepEqual(exp, seen) {
			t.Fatalf("got %v, expected %v", seen, exp)
		}
	}
}

func TestHasOverlappingReplica(t *testing.T) {
	defer leaktest.AfterTest(t)()
	stopper := stop.NewStopper()
	defer stopper.Stop(context.TODO())
	store, _ := createTestStore(t,
		testStoreOpts{
			// This test was written before test stores could start with more than one
			// range and was not adapted.
			createSystemRanges: false,
		},
		stopper)
	if _, err := store.GetReplica(0); err == nil {
		t.Error("expected GetRange to fail on missing range")
	}
	// Range 1 already exists. Make sure we can fetch it.
	repl1, err := store.GetReplica(1)
	if err != nil {
		t.Error(err)
	}
	// Remove range 1.
	if err := store.RemoveReplica(context.Background(), repl1, repl1.Desc().NextReplicaID, RemoveOptions{
		DestroyData: true,
	}); err != nil {
		t.Error(err)
	}

	// Create ranges.
	rngDescs := []struct {
		id         int
		start, end roachpb.RKey
	}{
		{2, roachpb.RKey("b"), roachpb.RKey("c")},
		{3, roachpb.RKey("c"), roachpb.RKey("d")},
		{4, roachpb.RKey("d"), roachpb.RKey("f")},
	}

	for _, desc := range rngDescs {
		repl := createReplica(store, roachpb.RangeID(desc.id), desc.start, desc.end)
		if err := store.AddReplica(repl); err != nil {
			t.Fatal(err)
		}
	}

	testCases := []struct {
		start, end roachpb.RKey
		exp        bool
	}{
		{roachpb.RKey("a"), roachpb.RKey("c"), true},
		{roachpb.RKey("b"), roachpb.RKey("c"), true},
		{roachpb.RKey("b"), roachpb.RKey("d"), true},
		{roachpb.RKey("d"), roachpb.RKey("e"), true},
		{roachpb.RKey("d"), roachpb.RKey("g"), true},
		{roachpb.RKey("e"), roachpb.RKey("e\x00"), true},

		{roachpb.RKey("f"), roachpb.RKey("g"), false},
		{roachpb.RKey("a"), roachpb.RKey("b"), false},
	}

	for i, test := range testCases {
		rngDesc := &roachpb.RangeDescriptor{StartKey: test.start, EndKey: test.end}
		if r := store.getOverlappingKeyRangeLocked(rngDesc) != nil; r != test.exp {
			t.Errorf("%d: expected range %v; got %v", i, test.exp, r)
		}
	}
}

func TestLookupPrecedingReplica(t *testing.T) {
	defer leaktest.AfterTest(t)()

	ctx := context.Background()
	stopper := stop.NewStopper()
	defer stopper.Stop(ctx)
	store, _ := createTestStore(t,
		testStoreOpts{
			// This test was written before test stores could start with more than one
			// range and was not adapted.
			createSystemRanges: false,
		},
		stopper)

	// Clobber the existing range so we can test ranges that aren't KeyMin or
	// KeyMax.
	repl1, err := store.GetReplica(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RemoveReplica(ctx, repl1, repl1.Desc().NextReplicaID, RemoveOptions{
		DestroyData: true,
	}); err != nil {
		t.Fatal(err)
	}

	repl2 := createReplica(store, 2, roachpb.RKey("a"), roachpb.RKey("b"))
	if err := store.AddReplica(repl2); err != nil {
		t.Fatal(err)
	}
	repl3 := createReplica(store, 3, roachpb.RKey("b"), roachpb.RKey("c"))
	if err := store.AddReplica(repl3); err != nil {
		t.Fatal(err)
	}
	if err := store.addPlaceholder(&ReplicaPlaceholder{rangeDesc: roachpb.RangeDescriptor{
		RangeID: 4, StartKey: roachpb.RKey("c"), EndKey: roachpb.RKey("d"),
	}}); err != nil {
		t.Fatal(err)
	}
	repl5 := createReplica(store, 5, roachpb.RKey("e"), roachpb.RKey("f"))
	if err := store.AddReplica(repl5); err != nil {
		t.Fatal(err)
	}

	for i, tc := range []struct {
		key     roachpb.RKey
		expRepl *Replica
	}{
		{roachpb.RKeyMin, nil},
		{roachpb.RKey("a"), nil},
		{roachpb.RKey("aa"), nil},
		{roachpb.RKey("b"), repl2},
		{roachpb.RKey("bb"), repl2},
		{roachpb.RKey("c"), repl3},
		{roachpb.RKey("cc"), repl3},
		{roachpb.RKey("d"), repl3},
		{roachpb.RKey("dd"), repl3},
		{roachpb.RKey("e"), repl3},
		{roachpb.RKey("ee"), repl3},
		{roachpb.RKey("f"), repl5},
		{roachpb.RKeyMax, repl5},
	} {
		if repl := store.lookupPrecedingReplica(tc.key); repl != tc.expRepl {
			t.Errorf("%d: expected replica %v; got %v", i, tc.expRepl, repl)
		}
	}
}

func TestMaybeMarkReplicaInitialized(t *testing.T) {
	defer leaktest.AfterTest(t)()
	stopper := stop.NewStopper()
	defer stopper.Stop(context.TODO())
	store, _ := createTestStore(t,
		testStoreOpts{
			// This test was written before test stores could start with more than one
			// range and was not adapted.
			createSystemRanges: false,
		},
		stopper)

	// Clobber the existing range so we can test overlaps that aren't KeyMin or KeyMax.
	repl1, err := store.GetReplica(1)
	if err != nil {
		t.Error(err)
	}
	if err := store.RemoveReplica(context.Background(), repl1, repl1.Desc().NextReplicaID, RemoveOptions{
		DestroyData: true,
	}); err != nil {
		t.Error(err)
	}

	repl := createReplica(store, roachpb.RangeID(2), roachpb.RKey("a"), roachpb.RKey("c"))
	if err := store.AddReplica(repl); err != nil {
		t.Fatal(err)
	}

	newRangeID := roachpb.RangeID(3)
	desc := &roachpb.RangeDescriptor{
		RangeID: newRangeID,
		InternalReplicas: []roachpb.ReplicaDescriptor{{
			NodeID:    1,
			StoreID:   1,
			ReplicaID: 1,
		}},
		NextReplicaID: 2,
	}

	r := &Replica{
		RangeID:   desc.RangeID,
		store:     store,
		abortSpan: abortspan.New(desc.RangeID),
	}
	if err := r.init(desc, store.Clock(), 0); err != nil {
		t.Fatal(err)
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	expectedResult := "attempted to process uninitialized range.*"
	ctx := r.AnnotateCtx(context.TODO())
	if err := store.maybeMarkReplicaInitializedLocked(ctx, r); !testutils.IsError(err, expectedResult) {
		t.Errorf("expected maybeMarkReplicaInitializedLocked with uninitialized replica to fail, got %v", err)
	}

	// Initialize the range with start and end keys.
	r.mu.Lock()
	r.mu.state.Desc.StartKey = roachpb.RKey("b")
	r.mu.state.Desc.EndKey = roachpb.RKey("d")
	r.mu.Unlock()

	if err := store.maybeMarkReplicaInitializedLocked(ctx, r); err != nil {
		t.Errorf("expected maybeMarkReplicaInitializedLocked on a replica that's not in the uninit map to silently succeed, got %v", err)
	}

	store.mu.uninitReplicas[newRangeID] = r

	expectedResult = ".*cannot initialize replica.*"
	if err := store.maybeMarkReplicaInitializedLocked(ctx, r); !testutils.IsError(err, expectedResult) {
		t.Errorf("expected maybeMarkReplicaInitializedLocked with overlapping keys to fail, got %v", err)
	}
}

// TestStoreSend verifies straightforward command execution
// of both a read-only and a read-write command.
func TestStoreSend(t *testing.T) {
	defer leaktest.AfterTest(t)()
	stopper := stop.NewStopper()
	defer stopper.Stop(context.TODO())
	store, _ := createTestStore(t, testStoreOpts{createSystemRanges: true}, stopper)
	gArgs := getArgs([]byte("a"))

	// Try a successful get request.
	if _, pErr := client.SendWrapped(context.Background(), store.TestSender(), &gArgs); pErr != nil {
		t.Fatal(pErr)
	}
	pArgs := putArgs([]byte("a"), []byte("aaa"))
	if _, pErr := client.SendWrapped(context.Background(), store.TestSender(), &pArgs); pErr != nil {
		t.Fatal(pErr)
	}
}

// TestStoreObservedTimestamp verifies that execution of a transactional
// command on a Store always returns a timestamp observation, either per the
// error's or the response's transaction, as well as an originating NodeID.
func TestStoreObservedTimestamp(t *testing.T) {
	defer leaktest.AfterTest(t)()
	badKey := []byte("a")
	goodKey := []byte("b")
	desc := roachpb.ReplicaDescriptor{
		NodeID: 5,
		// not relevant
		StoreID:   1,
		ReplicaID: 2,
	}

	testCases := []struct {
		key   roachpb.Key
		check func(int64, roachpb.Response, *roachpb.Error)
	}{
		{badKey,
			func(wallNanos int64, _ roachpb.Response, pErr *roachpb.Error) {
				if pErr == nil {
					t.Fatal("expected an error")
				}
				txn := pErr.GetTxn()
				if txn == nil || txn.ID == (uuid.UUID{}) {
					t.Fatalf("expected nontrivial transaction in %s", pErr)
				}
				if ts, _ := txn.GetObservedTimestamp(desc.NodeID); ts.WallTime != wallNanos {
					t.Fatalf("unexpected observed timestamps, expected %d->%d but got map %+v",
						desc.NodeID, wallNanos, txn.ObservedTimestamps)
				}
				if pErr.OriginNode != desc.NodeID {
					t.Fatalf("unexpected OriginNode %d, expected %d",
						pErr.OriginNode, desc.NodeID)
				}

			}},
		{goodKey,
			func(wallNanos int64, pReply roachpb.Response, pErr *roachpb.Error) {
				if pErr != nil {
					t.Fatal(pErr)
				}
				txn := pReply.Header().Txn
				if txn == nil || txn.ID == (uuid.UUID{}) {
					t.Fatal("expected transactional response")
				}
				obs, _ := txn.GetObservedTimestamp(desc.NodeID)
				if act, exp := obs.WallTime, wallNanos; exp != act {
					t.Fatalf("unexpected observed wall time: %d, wanted %d", act, exp)
				}
			}},
	}

	for _, test := range testCases {
		func() {
			manual := hlc.NewManualClock(123)
			cfg := TestStoreConfig(hlc.NewClock(manual.UnixNano, time.Nanosecond))
			cfg.TestingKnobs.EvalKnobs.TestingEvalFilter =
				func(filterArgs storagebase.FilterArgs) *roachpb.Error {
					if bytes.Equal(filterArgs.Req.Header().Key, badKey) {
						return roachpb.NewError(errors.Errorf("boom"))
					}
					return nil
				}
			stopper := stop.NewStopper()
			defer stopper.Stop(context.TODO())
			store := createTestStoreWithConfig(t, stopper, testStoreOpts{createSystemRanges: true}, &cfg)
			txn := newTransaction("test", test.key, 1, store.cfg.Clock)
			txn.MaxTimestamp = hlc.MaxTimestamp
			pArgs := putArgs(test.key, []byte("value"))
			h := roachpb.Header{
				Txn:     txn,
				Replica: desc,
			}
			assignSeqNumsForReqs(txn, &pArgs)
			pReply, pErr := client.SendWrappedWith(context.Background(), store.TestSender(), h, &pArgs)
			test.check(manual.UnixNano(), pReply, pErr)
		}()
	}
}

// TestStoreAnnotateNow verifies that the Store sets Now on the batch responses.
func TestStoreAnnotateNow(t *testing.T) {
	defer leaktest.AfterTest(t)()
	ctx := context.Background()
	badKey := []byte("a")
	goodKey := []byte("b")
	desc := roachpb.ReplicaDescriptor{
		NodeID: 5,
		// not relevant
		StoreID:   1,
		ReplicaID: 2,
	}

	testCases := []struct {
		key   roachpb.Key
		check func(*roachpb.BatchResponse, *roachpb.Error)
	}{
		{badKey,
			func(_ *roachpb.BatchResponse, pErr *roachpb.Error) {
				if pErr == nil {
					t.Fatal("expected an error")
				}
				if pErr.Now == (hlc.Timestamp{}) {
					t.Fatal("timestamp not annotated on error")
				}
			}},
		{goodKey,
			func(pReply *roachpb.BatchResponse, pErr *roachpb.Error) {
				if pErr != nil {
					t.Fatal(pErr)
				}
				if pReply.Now == (hlc.Timestamp{}) {
					t.Fatal("timestamp not annotated on batch response")
				}
			}},
	}

	testutils.RunTrueAndFalse(t, "useTxn", func(t *testing.T, useTxn bool) {
		for _, test := range testCases {
			t.Run(test.key.String(), func(t *testing.T) {
				cfg := TestStoreConfig(nil)
				cfg.TestingKnobs.EvalKnobs.TestingEvalFilter =
					func(filterArgs storagebase.FilterArgs) *roachpb.Error {
						if bytes.Equal(filterArgs.Req.Header().Key, badKey) {
							return roachpb.NewErrorWithTxn(errors.Errorf("boom"), filterArgs.Hdr.Txn)
						}
						return nil
					}
				stopper := stop.NewStopper()
				defer stopper.Stop(ctx)
				store := createTestStoreWithConfig(t, stopper, testStoreOpts{createSystemRanges: true}, &cfg)
				var txn *roachpb.Transaction
				pArgs := putArgs(test.key, []byte("value"))
				if useTxn {
					txn = newTransaction("test", test.key, 1, store.cfg.Clock)
					txn.MaxTimestamp = hlc.MaxTimestamp
					assignSeqNumsForReqs(txn, &pArgs)
				}
				ba := roachpb.BatchRequest{
					Header: roachpb.Header{
						Txn:     txn,
						Replica: desc,
					},
				}
				ba.Add(&pArgs)

				test.check(store.TestSender().Send(ctx, ba))
			})
		}
	})
}

// TestStoreVerifyKeys checks that key length is enforced and
// that end keys must sort >= start.
func TestStoreVerifyKeys(t *testing.T) {
	defer leaktest.AfterTest(t)()
	stopper := stop.NewStopper()
	defer stopper.Stop(context.TODO())
	store, _ := createTestStore(t, testStoreOpts{createSystemRanges: true}, stopper)
	// Try a start key == KeyMax.
	gArgs := getArgs(roachpb.KeyMax)
	if _, pErr := client.SendWrapped(context.Background(), store.TestSender(), &gArgs); !testutils.IsPError(pErr, "must be less than KeyMax") {
		t.Fatalf("expected error for start key == KeyMax: %v", pErr)
	}
	// Try a get with an end key specified (get requires only a start key and should fail).
	gArgs.EndKey = roachpb.KeyMax
	if _, pErr := client.SendWrapped(context.Background(), store.TestSender(), &gArgs); !testutils.IsPError(pErr, "must be less than KeyMax") {
		t.Fatalf("unexpected error for end key specified on a non-range-based operation: %v", pErr)
	}
	// Try a scan with end key < start key.
	sArgs := scanArgs([]byte("b"), []byte("a"))
	if _, pErr := client.SendWrapped(context.Background(), store.TestSender(), &sArgs); !testutils.IsPError(pErr, "must be greater than") {
		t.Fatalf("unexpected error for end key < start: %v", pErr)
	}
	// Try a scan with start key == end key.
	sArgs.Key = []byte("a")
	sArgs.EndKey = sArgs.Key
	if _, pErr := client.SendWrapped(context.Background(), store.TestSender(), &sArgs); !testutils.IsPError(pErr, "must be greater than") {
		t.Fatalf("unexpected error for start == end key: %v", pErr)
	}
	// Try a scan with range-local start key, but "regular" end key.
	sArgs.Key = keys.MakeRangeKey([]byte("test"), []byte("sffx"), nil)
	sArgs.EndKey = []byte("z")
	if _, pErr := client.SendWrapped(context.Background(), store.TestSender(), &sArgs); !testutils.IsPError(pErr, "range-local") {
		t.Fatalf("unexpected error for local start, non-local end key: %v", pErr)
	}

	// Try a put to meta2 key which would otherwise exceed maximum key
	// length, but is accepted because of the meta prefix.
	meta2KeyMax := testutils.MakeKey(keys.Meta2Prefix, roachpb.RKeyMax)
	pArgs := putArgs(meta2KeyMax, []byte("value"))
	if _, pErr := client.SendWrapped(context.Background(), store.TestSender(), &pArgs); pErr != nil {
		t.Fatalf("unexpected error on put to meta2 value: %s", pErr)
	}
	// Try to put a range descriptor record for a start key which is
	// maximum length.
	key := append([]byte{}, roachpb.RKeyMax...)
	key[len(key)-1] = 0x01
	pArgs = putArgs(keys.RangeDescriptorKey(key), []byte("value"))
	if _, pErr := client.SendWrapped(context.Background(), store.TestSender(), &pArgs); pErr != nil {
		t.Fatalf("unexpected error on put to range descriptor for KeyMax value: %s", pErr)
	}
	// Try a put to txn record for a meta2 key (note that this doesn't
	// actually happen in practice, as txn records are not put directly,
	// but are instead manipulated only through txn methods).
	pArgs = putArgs(keys.TransactionKey(meta2KeyMax, uuid.MakeV4()), []byte("value"))
	if _, pErr := client.SendWrapped(context.Background(), store.TestSender(), &pArgs); pErr != nil {
		t.Fatalf("unexpected error on put to txn meta2 value: %s", pErr)
	}
}

// TestStoreSendUpdateTime verifies that the node clock is updated.
func TestStoreSendUpdateTime(t *testing.T) {
	defer leaktest.AfterTest(t)()
	stopper := stop.NewStopper()
	defer stopper.Stop(context.TODO())
	store, _ := createTestStore(t, testStoreOpts{createSystemRanges: true}, stopper)
	args := getArgs([]byte("a"))
	reqTS := store.cfg.Clock.Now().Add(store.cfg.Clock.MaxOffset().Nanoseconds(), 0)
	_, pErr := client.SendWrappedWith(context.Background(), store.TestSender(), roachpb.Header{Timestamp: reqTS}, &args)
	if pErr != nil {
		t.Fatal(pErr)
	}
	ts := store.cfg.Clock.Now()
	if ts.WallTime != reqTS.WallTime || ts.Logical <= reqTS.Logical {
		t.Errorf("expected store clock to advance to %s; got %s", reqTS, ts)
	}
}

// TestStoreSendWithZeroTime verifies that no timestamp causes
// the command to assume the node's wall time.
func TestStoreSendWithZeroTime(t *testing.T) {
	defer leaktest.AfterTest(t)()
	stopper := stop.NewStopper()
	defer stopper.Stop(context.TODO())
	store, _ := createTestStore(t, testStoreOpts{createSystemRanges: true}, stopper)
	args := getArgs([]byte("a"))

	var ba roachpb.BatchRequest
	ba.Add(&args)
	br, pErr := store.TestSender().Send(context.Background(), ba)
	if pErr != nil {
		t.Fatal(pErr)
	}
	// The Logical time will increase over the course of the command
	// execution so we can only rely on comparing the WallTime.
	if br.Timestamp.WallTime != store.cfg.Clock.Now().WallTime {
		t.Errorf("expected reply to have store clock time %s; got %s",
			store.cfg.Clock.Now(), br.Timestamp)
	}
}

// TestStoreSendWithClockOffset verifies that if the request
// specifies a timestamp further into the future than the node's
// maximum allowed clock offset, the cmd fails.
func TestStoreSendWithClockOffset(t *testing.T) {
	defer leaktest.AfterTest(t)()
	stopper := stop.NewStopper()
	defer stopper.Stop(context.TODO())
	store, _ := createTestStore(t, testStoreOpts{createSystemRanges: true}, stopper)
	args := getArgs([]byte("a"))
	// Set args timestamp to exceed max offset.
	reqTS := store.cfg.Clock.Now().Add(store.cfg.Clock.MaxOffset().Nanoseconds()+1, 0)
	_, pErr := client.SendWrappedWith(context.Background(), store.TestSender(), roachpb.Header{Timestamp: reqTS}, &args)
	if !testutils.IsPError(pErr, "remote wall time is too far ahead") {
		t.Errorf("unexpected error: %v", pErr)
	}
}

// TestStoreSendBadRange passes a bad range.
func TestStoreSendBadRange(t *testing.T) {
	defer leaktest.AfterTest(t)()
	stopper := stop.NewStopper()
	defer stopper.Stop(context.TODO())
	store, _ := createTestStore(t, testStoreOpts{createSystemRanges: true}, stopper)
	args := getArgs([]byte("0"))
	if _, pErr := client.SendWrappedWith(context.Background(), store.TestSender(), roachpb.Header{
		RangeID: 2, // no such range
	}, &args); pErr == nil {
		t.Error("expected invalid range")
	}
}

// splitTestRange splits a range. This does *not* fully emulate a real split
// and should not be used in new tests. Tests that need splits should either live in
// client_split_test.go and use AdminSplit instead of this function or use the
// TestServerInterface.
// See #702
// TODO(bdarnell): convert tests that use this function to use AdminSplit instead.
func splitTestRange(store *Store, key, splitKey roachpb.RKey, t *testing.T) *Replica {
	repl := store.LookupReplica(key)
	if repl == nil {
		t.Fatalf("couldn't lookup range for key %q", key)
	}
	desc, err := store.NewRangeDescriptor(
		context.Background(), splitKey, repl.Desc().EndKey, repl.Desc().Replicas())
	if err != nil {
		t.Fatal(err)
	}
	// Minimal amount of work to keep this deprecated machinery working: Write
	// some required Raft keys.
	cv := store.ClusterSettings().Version.Version().Version
	if _, err := stateloader.WriteInitialState(
		context.Background(), store.engine, enginepb.MVCCStats{}, *desc, roachpb.Lease{},
		hlc.Timestamp{}, cv, stateloader.TruncatedStateUnreplicated,
	); err != nil {
		t.Fatal(err)
	}
	newRng, err := NewReplica(desc, store, 0)
	if err != nil {
		t.Fatal(err)
	}
	newLeftDesc := *repl.Desc()
	newLeftDesc.EndKey = splitKey
	if err = store.SplitRange(repl.AnnotateCtx(context.TODO()), repl, newRng, newLeftDesc); err != nil {
		t.Fatal(err)
	}
	return newRng
}

// TestStoreSendOutOfRange passes a key not contained
// within the range's key range.
func TestStoreSendOutOfRange(t *testing.T) {
	defer leaktest.AfterTest(t)()
	stopper := stop.NewStopper()
	defer stopper.Stop(context.TODO())
	store, _ := createTestStore(t, testStoreOpts{createSystemRanges: true}, stopper)

	repl2 := splitTestRange(store, roachpb.RKeyMin, roachpb.RKey(roachpb.Key("b")), t)

	// Range 1 is from KeyMin to "b", so reading "b" from range 1 should
	// fail because it's just after the range boundary.
	args := getArgs([]byte("b"))
	if _, err := client.SendWrappedWith(context.Background(), store.TestSender(), roachpb.Header{
		RangeID: 1,
	}, &args); err == nil {
		t.Error("expected key to be out of range")
	}

	// Range 2 is from "b" to KeyMax, so reading "a" from range 2 should
	// fail because it's before the start of the range.
	args = getArgs([]byte("a"))
	if _, err := client.SendWrappedWith(context.Background(), store.TestSender(), roachpb.Header{
		RangeID: repl2.RangeID,
	}, &args); err == nil {
		t.Error("expected key to be out of range")
	}
}

// TestStoreRangeIDAllocation verifies that  range IDs are
// allocated in successive blocks.
func TestStoreRangeIDAllocation(t *testing.T) {
	defer leaktest.AfterTest(t)()
	stopper := stop.NewStopper()
	defer stopper.Stop(context.TODO())
	store, _ := createTestStore(t,
		testStoreOpts{
			// This test was written before test stores could start with more than one
			// range and was not adapted.
			createSystemRanges: false,
		},
		stopper)

	// Range IDs should be allocated from ID 2 (first allocated range)
	// to rangeIDAllocCount * 3 + 1.
	for i := 0; i < rangeIDAllocCount*3; i++ {
		replicas := []roachpb.ReplicaDescriptor{{StoreID: store.StoreID()}}
		desc, err := store.NewRangeDescriptor(context.Background(),
			roachpb.RKey(fmt.Sprintf("%03d", i)), roachpb.RKey(fmt.Sprintf("%03d", i+1)),
			roachpb.MakeReplicaDescriptors(&replicas))
		if err != nil {
			t.Fatal(err)
		}
		if desc.RangeID != roachpb.RangeID(2+i) {
			t.Errorf("expected range id %d; got %d", 2+i, desc.RangeID)
		}
	}
}

// TestStoreReplicasByKey verifies we can lookup ranges by key using
// the sorted replicasByKey slice.
func TestStoreReplicasByKey(t *testing.T) {
	defer leaktest.AfterTest(t)()
	stopper := stop.NewStopper()
	defer stopper.Stop(context.TODO())
	store, _ := createTestStore(t,
		testStoreOpts{
			// This test was written before test stores could start with more than one
			// range and was not adapted.
			createSystemRanges: false,
		},
		stopper)

	r0 := store.LookupReplica(roachpb.RKeyMin)
	r1 := splitTestRange(store, roachpb.RKeyMin, roachpb.RKey("A"), t)
	r2 := splitTestRange(store, roachpb.RKey("A"), roachpb.RKey("C"), t)
	r3 := splitTestRange(store, roachpb.RKey("C"), roachpb.RKey("X"), t)
	r4 := splitTestRange(store, roachpb.RKey("X"), roachpb.RKey("ZZ"), t)

	if r := store.LookupReplica(roachpb.RKey("0")); r != r0 {
		t.Errorf("mismatched replica %s != %s", r, r0)
	}
	if r := store.LookupReplica(roachpb.RKey("B")); r != r1 {
		t.Errorf("mismatched replica %s != %s", r, r1)
	}
	if r := store.LookupReplica(roachpb.RKey("C")); r != r2 {
		t.Errorf("mismatched replica %s != %s", r, r2)
	}
	if r := store.LookupReplica(roachpb.RKey("M")); r != r2 {
		t.Errorf("mismatched replica %s != %s", r, r2)
	}
	if r := store.LookupReplica(roachpb.RKey("X")); r != r3 {
		t.Errorf("mismatched replica %s != %s", r, r3)
	}
	if r := store.LookupReplica(roachpb.RKey("Z")); r != r3 {
		t.Errorf("mismatched replica %s != %s", r, r3)
	}
	if r := store.LookupReplica(roachpb.RKey("ZZ")); r != r4 {
		t.Errorf("mismatched replica %s != %s", r, r4)
	}
	if r := store.LookupReplica(roachpb.RKey("\xff\x00")); r != r4 {
		t.Errorf("mismatched replica %s != %s", r, r4)
	}
	if store.LookupReplica(roachpb.RKeyMax) != nil {
		t.Errorf("expected roachpb.KeyMax to not have an associated replica")
	}
}

// TestStoreSetRangesMaxBytes creates a set of ranges via splitting
// and then sets the config zone to a custom max bytes value to
// verify the ranges' max bytes are updated appropriately.
func TestStoreSetRangesMaxBytes(t *testing.T) {
	defer leaktest.AfterTest(t)()
	stopper := stop.NewStopper()
	defer stopper.Stop(context.TODO())
	cfg := TestStoreConfig(nil)
	cfg.TestingKnobs.DisableMergeQueue = true
	store := createTestStoreWithConfig(t, stopper,
		testStoreOpts{
			// This test was written before test stores could start with more than one
			// range and was not adapted.
			createSystemRanges: false,
		},
		&cfg)

	baseID := uint32(keys.MinUserDescID)
	testData := []struct {
		repl        *Replica
		expMaxBytes int64
	}{
		{store.LookupReplica(roachpb.RKeyMin),
			*store.cfg.DefaultZoneConfig.RangeMaxBytes},
		{splitTestRange(store, roachpb.RKeyMin, keys.MakeTablePrefix(baseID), t),
			1 << 20},
		{splitTestRange(store, keys.MakeTablePrefix(baseID), keys.MakeTablePrefix(baseID+1), t),
			*store.cfg.DefaultZoneConfig.RangeMaxBytes},
		{splitTestRange(store, keys.MakeTablePrefix(baseID+1), keys.MakeTablePrefix(baseID+2), t),
			2 << 20},
	}

	// Set zone configs.
	config.TestingSetZoneConfig(baseID, config.ZoneConfig{RangeMaxBytes: proto.Int64(1 << 20)})
	config.TestingSetZoneConfig(baseID+2, config.ZoneConfig{RangeMaxBytes: proto.Int64(2 << 20)})

	// Despite faking the zone configs, we still need to have a system config
	// entry so that the store picks up the new zone configs. This new system
	// config needs to be non-empty so that it differs from the initial value
	// which triggers the system config callback to be run.
	sysCfg := &config.SystemConfigEntries{}
	sysCfg.Values = []roachpb.KeyValue{{Key: roachpb.Key("a")}}
	if err := store.Gossip().AddInfoProto(gossip.KeySystemConfig, sysCfg, 0); err != nil {
		t.Fatal(err)
	}

	testutils.SucceedsSoon(t, func() error {
		for _, test := range testData {
			if mb := test.repl.GetMaxBytes(); mb != test.expMaxBytes {
				return errors.Errorf("range max bytes values did not change to %d; got %d", test.expMaxBytes, mb)
			}
		}
		return nil
	})
}

// TestStoreResolveWriteIntent adds a write intent and then verifies
// that a put returns success and aborts intent's txn in the event the
// pushee has lower priority. Otherwise, verifies that the put blocks
// until the original txn is ended.
func TestStoreResolveWriteIntent(t *testing.T) {
	defer leaktest.AfterTest(t)()

	manual := hlc.NewManualClock(123)
	cfg := TestStoreConfig(hlc.NewClock(manual.UnixNano, time.Nanosecond))
	cfg.TestingKnobs.EvalKnobs.TestingEvalFilter =
		func(filterArgs storagebase.FilterArgs) *roachpb.Error {
			pr, ok := filterArgs.Req.(*roachpb.PushTxnRequest)
			if !ok || pr.PusherTxn.Name != "test" {
				return nil
			}
			if exp, act := manual.UnixNano(), pr.PushTo.WallTime; exp > act {
				return roachpb.NewError(fmt.Errorf("expected PushTo > WallTime, but got %d < %d:\n%+v", act, exp, pr))
			}
			return nil
		}
	stopper := stop.NewStopper()
	defer stopper.Stop(context.TODO())
	store := createTestStoreWithConfig(t, stopper, testStoreOpts{createSystemRanges: true}, &cfg)

	for i, resolvable := range []bool{true, false} {
		key := roachpb.Key(fmt.Sprintf("key-%d", i))
		pusher := newTransaction("test", key, 1, store.cfg.Clock)
		pushee := newTransaction("test", key, 1, store.cfg.Clock)
		if resolvable {
			pushee.Priority = enginepb.MinTxnPriority
			pusher.Priority = enginepb.MaxTxnPriority // Pusher will win.
		} else {
			pushee.Priority = enginepb.MaxTxnPriority
			pusher.Priority = enginepb.MinTxnPriority // Pusher will lose.
		}

		// First lay down intent using the pushee's txn.
		pArgs := putArgs(key, []byte("value"))
		h := roachpb.Header{Txn: pushee}
		assignSeqNumsForReqs(pushee, &pArgs)
		if _, err := client.SendWrappedWith(context.Background(), store.TestSender(), h, &pArgs); err != nil {
			t.Fatal(err)
		}

		manual.Increment(100)
		// Now, try a put using the pusher's txn.
		h.Txn = pusher
		h.DeferWriteTooOldError = true
		resultCh := make(chan *roachpb.Error, 1)
		go func() {
			_, pErr := client.SendWrappedWith(context.Background(), store.TestSender(), h, &pArgs)
			resultCh <- pErr
		}()

		if resolvable {
			if pErr := <-resultCh; pErr != nil {
				t.Fatalf("expected intent resolved; got unexpected error: %s", pErr)
			}
			txnKey := keys.TransactionKey(pushee.Key, pushee.ID)
			var txn roachpb.Transaction
			if ok, err := engine.MVCCGetProto(
				context.Background(), store.Engine(), txnKey, hlc.Timestamp{}, &txn, engine.MVCCGetOptions{},
			); err != nil {
				t.Fatal(err)
			} else if ok {
				t.Fatalf("expected transaction record; got %s", txn)
			}
		} else {
			select {
			case pErr := <-resultCh:
				t.Fatalf("did not expect put to complete with lower priority: %s", pErr)
			case <-time.After(10 * time.Millisecond):
				// Send an end transaction to allow the original push to complete.
				etArgs, h := endTxnArgs(pushee, true)
				assignSeqNumsForReqs(pushee, &etArgs)
				if _, pErr := client.SendWrappedWith(context.Background(), store.TestSender(), h, &etArgs); pErr != nil {
					t.Fatal(pErr)
				}
				if pErr := <-resultCh; pErr != nil {
					t.Fatalf("expected successful put after pushee txn ended; got %s", pErr)
				}
			}
		}
	}
}

// TestStoreResolveWriteIntentRollback verifies that resolving a write
// intent by aborting it yields the previous value.
func TestStoreResolveWriteIntentRollback(t *testing.T) {
	defer leaktest.AfterTest(t)()
	stopper := stop.NewStopper()
	defer stopper.Stop(context.TODO())
	store, _ := createTestStore(t, testStoreOpts{createSystemRanges: true}, stopper)

	key := roachpb.Key("a")
	pusher := newTransaction("test", key, 1, store.cfg.Clock)
	pushee := newTransaction("test", key, 1, store.cfg.Clock)
	pushee.Priority = enginepb.MinTxnPriority
	pusher.Priority = enginepb.MaxTxnPriority // Pusher will win.

	// First lay down intent using the pushee's txn.
	args := incrementArgs(key, 1)
	h := roachpb.Header{Txn: pushee}
	assignSeqNumsForReqs(pushee, &args)
	if _, pErr := client.SendWrappedWith(context.Background(), store.TestSender(), h, &args); pErr != nil {
		t.Fatal(pErr)
	}

	// Now, try a put using the pusher's txn.
	h.Txn = pusher
	args.Increment = 2
	assignSeqNumsForReqs(pusher, &args)
	if resp, pErr := client.SendWrappedWith(context.Background(), store.TestSender(), h, &args); pErr != nil {
		t.Errorf("expected increment to succeed: %s", pErr)
	} else if reply := resp.(*roachpb.IncrementResponse); reply.NewValue != 2 {
		t.Errorf("expected rollback of earlier increment to yield increment value of 2; got %d", reply.NewValue)
	}
}

// TestStoreResolveWriteIntentPushOnRead verifies that resolving a write intent
// for a read will push the timestamp. It tests this along a few dimensions:
// - high-priority pushes       vs. low-priority pushes
// - already pushed pushee txns vs. not already pushed pushee txns
// - PENDING pushee txn records vs. STAGING pushee txn records
func TestStoreResolveWriteIntentPushOnRead(t *testing.T) {
	defer leaktest.AfterTest(t)()
	storeCfg := TestStoreConfig(nil)
	storeCfg.TestingKnobs.DontRetryPushTxnFailures = true
	storeCfg.TestingKnobs.DontRecoverIndeterminateCommits = true
	stopper := stop.NewStopper()
	defer stopper.Stop(context.TODO())
	store := createTestStoreWithConfig(t, stopper, testStoreOpts{createSystemRanges: true}, &storeCfg)

	testCases := []struct {
		pusherWillWin       bool   // if true, pusher will have a high enough priority to push the pushee
		pusheeAlreadyPushed bool   // if true, pushee's timestamp will be set above pusher's target timestamp
		pusheeStagingRecord bool   // if true, pushee's record is STAGING, otherwise PENDING
		expPushError        string // regexp pattern to match on run error, if not empty
		expPusheeRetry      bool   // do we expect the pushee to hit a retry error when committing?
	}{
		{
			// Insufficient priority to push.
			pusherWillWin:       false,
			pusheeAlreadyPushed: false,
			pusheeStagingRecord: false,
			expPushError:        "failed to push",
			expPusheeRetry:      false,
		},
		{
			// Successful push.
			pusherWillWin:       true,
			pusheeAlreadyPushed: false,
			pusheeStagingRecord: false,
			expPushError:        "",
			expPusheeRetry:      true,
		},
		{
			// Already pushed, no-op.
			pusherWillWin:       false,
			pusheeAlreadyPushed: true,
			pusheeStagingRecord: false,
			expPushError:        "",
			expPusheeRetry:      false,
		},
		{
			// Already pushed, no-op.
			pusherWillWin:       true,
			pusheeAlreadyPushed: true,
			pusheeStagingRecord: false,
			expPushError:        "",
			expPusheeRetry:      false,
		},
		{
			// Insufficient priority to push.
			pusherWillWin:       false,
			pusheeAlreadyPushed: false,
			pusheeStagingRecord: true,
			expPushError:        "failed to push",
			expPusheeRetry:      false,
		},
		{
			// Cannot push STAGING txn record.
			pusherWillWin:       true,
			pusheeAlreadyPushed: false,
			pusheeStagingRecord: true,
			expPushError:        "found txn in indeterminate STAGING state",
			expPusheeRetry:      false,
		},
		{
			// Already pushed the STAGING record, no-op.
			pusherWillWin:       false,
			pusheeAlreadyPushed: true,
			pusheeStagingRecord: true,
			expPushError:        "",
			expPusheeRetry:      false,
		},
		{
			// Already pushed the STAGING record, no-op.
			pusherWillWin:       true,
			pusheeAlreadyPushed: true,
			pusheeStagingRecord: true,
			expPushError:        "",
			expPusheeRetry:      false,
		},
	}
	for _, tc := range testCases {
		name := fmt.Sprintf("pusherWillWin=%t,pusheePushed=%t,pusheeStaging=%t",
			tc.pusherWillWin, tc.pusheeAlreadyPushed, tc.pusheeStagingRecord)
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			key := roachpb.Key(fmt.Sprintf("key-%s", name))
			pusher := newTransaction("pusher", key, 1, store.cfg.Clock)
			pushee := newTransaction("pushee", key, 1, store.cfg.Clock)

			// Set transaction priorities.
			if tc.pusherWillWin {
				pushee.Priority = enginepb.MinTxnPriority
				pusher.Priority = enginepb.MaxTxnPriority // Pusher will win.
			} else {
				pushee.Priority = enginepb.MaxTxnPriority
				pusher.Priority = enginepb.MinTxnPriority // Pusher will lose.
			}

			// First, write original value.
			{
				args := putArgs(key, []byte("value1"))
				if _, pErr := client.SendWrapped(ctx, store.TestSender(), &args); pErr != nil {
					t.Fatal(pErr)
				}
			}

			// Second, lay down intent using the pushee's txn.
			{
				args := putArgs(key, []byte("value2"))
				assignSeqNumsForReqs(pushee, &args)
				h := roachpb.Header{Txn: pushee, DeferWriteTooOldError: true}
				if _, pErr := client.SendWrappedWith(ctx, store.TestSender(), h, &args); pErr != nil {
					t.Fatal(pErr)
				}
			}

			// Determine the timestamp to read at.
			readTs := store.cfg.Clock.Now()
			// Give the pusher a previous observed timestamp equal to this read
			// timestamp. This ensures that the pusher doesn't need to push the
			// intent any higher just to push it out of its uncertainty window.
			pusher.UpdateObservedTimestamp(store.Ident.NodeID, readTs)

			// If the pushee is already pushed, update the transaction record.
			if tc.pusheeAlreadyPushed {
				pushedTs := store.cfg.Clock.Now()
				pushee.Timestamp.Forward(pushedTs)
				pushee.RefreshedTimestamp.Forward(pushedTs)
				hb, hbH := heartbeatArgs(pushee, store.cfg.Clock.Now())
				if _, pErr := client.SendWrappedWith(ctx, store.TestSender(), hbH, &hb); pErr != nil {
					t.Fatal(pErr)
				}
			}

			// If the pushee is staging, update the transaction record.
			if tc.pusheeStagingRecord {
				et, etH := endTxnArgs(pushee, true)
				et.InFlightWrites = []roachpb.SequencedWrite{{Key: []byte("keyA"), Sequence: 1}}
				etReply, pErr := client.SendWrappedWith(ctx, store.TestSender(), etH, &et)
				if pErr != nil {
					t.Fatal(pErr)
				}
				if replyTxn := etReply.Header().Txn; replyTxn.Status != roachpb.STAGING {
					t.Fatalf("expected STAGING txn, found %v", replyTxn)
				}
			}

			// Now, try to read value using the pusher's txn.
			pusher.OrigTimestamp.Forward(readTs)
			pusher.Timestamp.Forward(readTs)
			gArgs := getArgs(key)
			assignSeqNumsForReqs(pusher, &gArgs)
			repl, pErr := client.SendWrappedWith(ctx, store.TestSender(), roachpb.Header{Txn: pusher}, &gArgs)
			if tc.expPushError == "" {
				if pErr != nil {
					t.Errorf("expected read to succeed: %s", pErr)
				} else if replyBytes, err := repl.(*roachpb.GetResponse).Value.GetBytes(); err != nil {
					t.Fatal(err)
				} else if !bytes.Equal(replyBytes, []byte("value1")) {
					t.Errorf("expected bytes to be %q, got %q", "value1", replyBytes)
				}
			} else {
				if !testutils.IsPError(pErr, tc.expPushError) {
					t.Fatalf("expected error %q, found %v", tc.expPushError, pErr)
				}
			}

			// Finally, try to end the pushee's transaction. Check whether
			// the commit succeeds or fails.
			etArgs, etH := endTxnArgs(pushee, true)
			assignSeqNumsForReqs(pushee, &etArgs)
			_, pErr = client.SendWrappedWith(ctx, store.TestSender(), etH, &etArgs)
			if tc.expPusheeRetry {
				if _, ok := pErr.GetDetail().(*roachpb.TransactionRetryError); !ok {
					t.Errorf("expected transaction retry error; got %s", pErr)
				}
			} else {
				if pErr != nil {
					t.Fatalf("expected no commit error; got %s", pErr)
				}
			}
		})
	}
}

// TestStoreResolveWriteIntentNoTxn verifies that reads and writes
// which are not part of a transaction can push intents.
func TestStoreResolveWriteIntentNoTxn(t *testing.T) {
	defer leaktest.AfterTest(t)()
	stopper := stop.NewStopper()
	defer stopper.Stop(context.TODO())
	store, _ := createTestStore(t, testStoreOpts{createSystemRanges: true}, stopper)

	key := roachpb.Key("a")
	pushee := newTransaction("test", key, 1, store.cfg.Clock)

	// First, write the pushee's txn via HeartbeatTxn request.
	hb, hbH := heartbeatArgs(pushee, pushee.Timestamp)
	if _, pErr := client.SendWrappedWith(context.Background(), store.TestSender(), hbH, &hb); pErr != nil {
		t.Fatal(pErr)
	}

	// Next, lay down intent from pushee.
	args := putArgs(key, []byte("value1"))
	assignSeqNumsForReqs(pushee, &args)
	if _, pErr := client.SendWrappedWith(context.Background(), store.TestSender(), hbH, &args); pErr != nil {
		t.Fatal(pErr)
	}

	// Now, try to read outside a transaction.
	getTS := store.cfg.Clock.Now() // accessed later
	{
		gArgs := getArgs(key)
		if reply, pErr := client.SendWrappedWith(context.Background(), store.TestSender(), roachpb.Header{
			Timestamp:    getTS,
			UserPriority: roachpb.MaxUserPriority,
		}, &gArgs); pErr != nil {
			t.Errorf("expected read to succeed: %s", pErr)
		} else if gReply := reply.(*roachpb.GetResponse); gReply.Value != nil {
			t.Errorf("expected value to be nil, got %+v", gReply.Value)
		}
	}

	{
		// Next, try to write outside of a transaction. We will succeed in pushing txn.
		putTS := store.cfg.Clock.Now()
		args.Value.SetBytes([]byte("value2"))
		if _, pErr := client.SendWrappedWith(context.Background(), store.TestSender(), roachpb.Header{
			Timestamp:    putTS,
			UserPriority: roachpb.MaxUserPriority,
		}, &args); pErr != nil {
			t.Errorf("expected success aborting pushee's txn; got %s", pErr)
		}
	}

	// Read pushee's txn.
	txnKey := keys.TransactionKey(pushee.Key, pushee.ID)
	var txn roachpb.Transaction
	if ok, err := engine.MVCCGetProto(
		context.Background(), store.Engine(), txnKey, hlc.Timestamp{}, &txn, engine.MVCCGetOptions{},
	); !ok || err != nil {
		t.Fatalf("not found or err: %+v", err)
	}
	if txn.Status != roachpb.ABORTED {
		t.Errorf("expected pushee to be aborted; got %s", txn.Status)
	}

	// Verify that the pushee's timestamp was moved forward on
	// former read, since we have it available in write intent error.
	minExpTS := getTS
	minExpTS.Logical++
	if txn.Timestamp.Less(minExpTS) {
		t.Errorf("expected pushee timestamp pushed to %s; got %s", minExpTS, txn.Timestamp)
	}
	// Similarly, verify that pushee's priority was moved from 0
	// to MaxTxnPriority-1 during push.
	if txn.Priority != enginepb.MaxTxnPriority-1 {
		t.Errorf("expected pushee priority to be pushed to %d; got %d", enginepb.MaxTxnPriority-1, txn.Priority)
	}

	// Finally, try to end the pushee's transaction; it should have
	// been aborted.
	etArgs, h := endTxnArgs(pushee, true)
	assignSeqNumsForReqs(pushee, &etArgs)
	_, pErr := client.SendWrappedWith(context.Background(), store.TestSender(), h, &etArgs)
	if pErr == nil {
		t.Errorf("unexpected success committing transaction")
	}
	if _, ok := pErr.GetDetail().(*roachpb.TransactionAbortedError); !ok {
		t.Errorf("expected transaction aborted error; got %s", pErr)
	}
}

func setTxnAutoGC(to bool) func() {
	orig := batcheval.TxnAutoGC
	f := func() {
		batcheval.TxnAutoGC = orig
	}
	batcheval.TxnAutoGC = to
	return f
}

// TestStoreReadInconsistent verifies that gets and scans with read
// consistency set to INCONSISTENT or READ_UNCOMMITTED either push or
// simply ignore extant intents (if they cannot be pushed), depending
// on the intent priority. READ_UNCOMMITTED requests will also return
// the intents that they run into.
func TestStoreReadInconsistent(t *testing.T) {
	defer leaktest.AfterTest(t)()

	for _, rc := range []roachpb.ReadConsistencyType{
		roachpb.READ_UNCOMMITTED,
		roachpb.INCONSISTENT,
	} {
		t.Run(rc.String(), func(t *testing.T) {
			// The test relies on being able to commit a Txn without specifying the
			// intent, while preserving the Txn record. Turn off
			// automatic cleanup for this to work.
			defer setTxnAutoGC(false)()
			stopper := stop.NewStopper()
			defer stopper.Stop(context.TODO())
			store, _ := createTestStore(t, testStoreOpts{createSystemRanges: true}, stopper)

			for _, canPush := range []bool{true, false} {
				keyA := roachpb.Key(fmt.Sprintf("%t-a", canPush))
				keyB := roachpb.Key(fmt.Sprintf("%t-b", canPush))

				// First, write keyA.
				args := putArgs(keyA, []byte("value1"))
				if _, pErr := client.SendWrapped(context.Background(), store.TestSender(), &args); pErr != nil {
					t.Fatal(pErr)
				}

				// Next, write intents for keyA and keyB. Note that the
				// transactions have unpushable priorities if canPush is true and
				// very pushable ones otherwise.
				priority := roachpb.UserPriority(-math.MaxInt32)
				if canPush {
					priority = -1
				}
				args.Value.SetBytes([]byte("value2"))
				txnA := newTransaction("testA", keyA, priority, store.cfg.Clock)
				txnB := newTransaction("testB", keyB, priority, store.cfg.Clock)
				for _, txn := range []*roachpb.Transaction{txnA, txnB} {
					args.Key = txn.Key
					assignSeqNumsForReqs(txn, &args)
					if _, pErr := client.SendWrappedWith(context.Background(), store.TestSender(), roachpb.Header{Txn: txn}, &args); pErr != nil {
						t.Fatal(pErr)
					}
				}
				// End txn B, but without resolving the intent.
				etArgs, h := endTxnArgs(txnB, true)
				assignSeqNumsForReqs(txnB, &etArgs)
				if _, pErr := client.SendWrappedWith(context.Background(), store.TestSender(), h, &etArgs); pErr != nil {
					t.Fatal(pErr)
				}

				// Now, get from both keys and verify. Whether we can push or not, we
				// will be able to read with both INCONSISTENT and READ_UNCOMMITTED.
				// With READ_UNCOMMITTED, we'll also be able to see the intent's value.
				gArgs := getArgs(keyA)
				if reply, pErr := client.SendWrappedWith(context.Background(), store.TestSender(), roachpb.Header{
					ReadConsistency: rc,
				}, &gArgs); pErr != nil {
					t.Errorf("expected read to succeed: %s", pErr)
				} else {
					gReply := reply.(*roachpb.GetResponse)
					if replyBytes, err := gReply.Value.GetBytes(); err != nil {
						t.Fatal(err)
					} else if !bytes.Equal(replyBytes, []byte("value1")) {
						t.Errorf("expected value %q, got %+v", []byte("value1"), reply)
					} else if rc == roachpb.READ_UNCOMMITTED {
						// READ_UNCOMMITTED will also return the intent.
						if replyIntentBytes, err := gReply.IntentValue.GetBytes(); err != nil {
							t.Fatal(err)
						} else if !bytes.Equal(replyIntentBytes, []byte("value2")) {
							t.Errorf("expected value %q, got %+v", []byte("value2"), reply)
						}
					} else if rc == roachpb.INCONSISTENT {
						if gReply.IntentValue != nil {
							t.Errorf("expected value nil, got %+v", gReply.IntentValue)
						}
					}
				}

				gArgs.Key = keyB
				if reply, pErr := client.SendWrappedWith(context.Background(), store.TestSender(), roachpb.Header{
					ReadConsistency: rc,
				}, &gArgs); pErr != nil {
					t.Errorf("expected read to succeed: %s", pErr)
				} else {
					gReply := reply.(*roachpb.GetResponse)
					if gReply.Value != nil {
						// The new value of B will not be read at first.
						t.Errorf("expected value nil, got %+v", gReply.Value)
					} else if rc == roachpb.READ_UNCOMMITTED {
						// READ_UNCOMMITTED will also return the intent.
						if replyIntentBytes, err := gReply.IntentValue.GetBytes(); err != nil {
							t.Fatal(err)
						} else if !bytes.Equal(replyIntentBytes, []byte("value2")) {
							t.Errorf("expected value %q, got %+v", []byte("value2"), reply)
						}
					} else if rc == roachpb.INCONSISTENT {
						if gReply.IntentValue != nil {
							t.Errorf("expected value nil, got %+v", gReply.IntentValue)
						}
					}
				}
				// However, it will be read eventually, as B's intent can be
				// resolved asynchronously as txn B is committed.
				testutils.SucceedsSoon(t, func() error {
					if reply, pErr := client.SendWrappedWith(context.Background(), store.TestSender(), roachpb.Header{
						ReadConsistency: rc,
					}, &gArgs); pErr != nil {
						return errors.Errorf("expected read to succeed: %s", pErr)
					} else if gReply := reply.(*roachpb.GetResponse).Value; gReply == nil {
						return errors.Errorf("value is nil")
					} else if replyBytes, err := gReply.GetBytes(); err != nil {
						return err
					} else if !bytes.Equal(replyBytes, []byte("value2")) {
						return errors.Errorf("expected value %q, got %+v", []byte("value2"), reply)
					}
					return nil
				})

				// Scan keys and verify results.
				sArgs := scanArgs(keyA, keyB.Next())
				reply, pErr := client.SendWrappedWith(context.Background(), store.TestSender(), roachpb.Header{
					ReadConsistency: rc,
				}, &sArgs)
				if pErr != nil {
					t.Errorf("expected scan to succeed: %s", pErr)
				}
				sReply := reply.(*roachpb.ScanResponse)
				if l := len(sReply.Rows); l != 2 {
					t.Errorf("expected 2 results; got %d", l)
				} else if key := sReply.Rows[0].Key; !key.Equal(keyA) {
					t.Errorf("expected key %q; got %q", keyA, key)
				} else if key := sReply.Rows[1].Key; !key.Equal(keyB) {
					t.Errorf("expected key %q; got %q", keyB, key)
				} else if val1, err := sReply.Rows[0].Value.GetBytes(); err != nil {
					t.Fatal(err)
				} else if !bytes.Equal(val1, []byte("value1")) {
					t.Errorf("expected value %q, got %q", []byte("value1"), val1)
				} else if val2, err := sReply.Rows[1].Value.GetBytes(); err != nil {
					t.Fatal(err)
				} else if !bytes.Equal(val2, []byte("value2")) {
					t.Errorf("expected value %q, got %q", []byte("value2"), val2)
				} else if rc == roachpb.READ_UNCOMMITTED {
					if l := len(sReply.IntentRows); l != 1 {
						t.Errorf("expected 1 intent result; got %d", l)
					} else if intentKey := sReply.IntentRows[0].Key; !intentKey.Equal(keyA) {
						t.Errorf("expected intent key %q; got %q", keyA, intentKey)
					} else if intentVal1, err := sReply.IntentRows[0].Value.GetBytes(); err != nil {
						t.Fatal(err)
					} else if !bytes.Equal(intentVal1, []byte("value2")) {
						t.Errorf("expected intent value %q, got %q", []byte("value2"), intentVal1)
					}
				} else if rc == roachpb.INCONSISTENT {
					if l := len(sReply.IntentRows); l != 0 {
						t.Errorf("expected 0 intent result; got %d", l)
					}
				}

				// Reverse scan keys and verify results.
				rsArgs := reverseScanArgs(keyA, keyB.Next())
				reply, pErr = client.SendWrappedWith(context.Background(), store.TestSender(), roachpb.Header{
					ReadConsistency: rc,
				}, &rsArgs)
				if pErr != nil {
					t.Errorf("expected scan to succeed: %s", pErr)
				}
				rsReply := reply.(*roachpb.ReverseScanResponse)
				if l := len(rsReply.Rows); l != 2 {
					t.Errorf("expected 2 results; got %d", l)
				} else if key := rsReply.Rows[0].Key; !key.Equal(keyB) {
					t.Errorf("expected key %q; got %q", keyA, key)
				} else if key := rsReply.Rows[1].Key; !key.Equal(keyA) {
					t.Errorf("expected key %q; got %q", keyB, key)
				} else if val1, err := rsReply.Rows[0].Value.GetBytes(); err != nil {
					t.Fatal(err)
				} else if !bytes.Equal(val1, []byte("value2")) {
					t.Errorf("expected value %q, got %q", []byte("value2"), val1)
				} else if val2, err := rsReply.Rows[1].Value.GetBytes(); err != nil {
					t.Fatal(err)
				} else if !bytes.Equal(val2, []byte("value1")) {
					t.Errorf("expected value %q, got %q", []byte("value1"), val2)
				} else if rc == roachpb.READ_UNCOMMITTED {
					if l := len(rsReply.IntentRows); l != 1 {
						t.Errorf("expected 1 intent result; got %d", l)
					} else if intentKey := rsReply.IntentRows[0].Key; !intentKey.Equal(keyA) {
						t.Errorf("expected intent key %q; got %q", keyA, intentKey)
					} else if intentVal1, err := rsReply.IntentRows[0].Value.GetBytes(); err != nil {
						t.Fatal(err)
					} else if !bytes.Equal(intentVal1, []byte("value2")) {
						t.Errorf("expected intent value %q, got %q", []byte("value2"), intentVal1)
					}
				} else if rc == roachpb.INCONSISTENT {
					if l := len(rsReply.IntentRows); l != 0 {
						t.Errorf("expected 0 intent result; got %d", l)
					}
				}
			}
		})
	}
}

// TestStoreScanResumeTSCache verifies that the timestamp cache is
// properly updated when scans and reverse scans return partial
// results and a resume span.
func TestStoreScanResumeTSCache(t *testing.T) {
	defer leaktest.AfterTest(t)()

	stopper := stop.NewStopper()
	defer stopper.Stop(context.TODO())
	store, manualClock := createTestStore(t, testStoreOpts{createSystemRanges: true}, stopper)

	// Write three keys at time t0.
	t0 := 1 * time.Second
	manualClock.Set(t0.Nanoseconds())
	h := roachpb.Header{Timestamp: makeTS(t0.Nanoseconds(), 0)}
	for _, keyStr := range []string{"a", "b", "c"} {
		key := roachpb.Key(keyStr)
		putArgs := putArgs(key, []byte("value"))
		if _, pErr := client.SendWrappedWith(context.Background(), store.TestSender(), h, &putArgs); pErr != nil {
			t.Fatal(pErr)
		}
	}

	// Scan the span at t1 with max keys and verify the expected resume span.
	span := roachpb.Span{Key: roachpb.Key("a"), EndKey: roachpb.Key("d")}
	sArgs := scanArgs(span.Key, span.EndKey)
	t1 := 2 * time.Second
	manualClock.Set(t1.Nanoseconds())
	h.Timestamp = makeTS(t1.Nanoseconds(), 0)
	h.MaxSpanRequestKeys = 2
	reply, pErr := client.SendWrappedWith(context.Background(), store.TestSender(), h, &sArgs)
	if pErr != nil {
		t.Fatal(pErr)
	}
	sReply := reply.(*roachpb.ScanResponse)
	if a, e := len(sReply.Rows), 2; a != e {
		t.Errorf("expected %d rows; got %d", e, a)
	}
	expResumeSpan := &roachpb.Span{Key: roachpb.Key("c"), EndKey: roachpb.Key("d")}
	if a, e := sReply.ResumeSpan, expResumeSpan; !reflect.DeepEqual(a, e) {
		t.Errorf("expected resume span %s; got %s", e, a)
	}

	// Verify the timestamp cache has been set for "b".Next(), but not for "c".
	rTS, _ := store.tsCache.GetMaxRead(roachpb.Key("b").Next(), nil)
	if a, e := rTS, makeTS(t1.Nanoseconds(), 0); a != e {
		t.Errorf("expected timestamp cache for \"b\".Next() set to %s; got %s", e, a)
	}
	rTS, _ = store.tsCache.GetMaxRead(roachpb.Key("c"), nil)
	if a, lt := rTS, makeTS(t1.Nanoseconds(), 0); !a.Less(lt) {
		t.Errorf("expected timestamp cache for \"c\" set less than %s; got %s", lt, a)
	}

	// Reverse scan the span at t1 with max keys and verify the expected resume span.
	t2 := 3 * time.Second
	manualClock.Set(t2.Nanoseconds())
	h.Timestamp = makeTS(t2.Nanoseconds(), 0)
	rsArgs := reverseScanArgs(span.Key, span.EndKey)
	reply, pErr = client.SendWrappedWith(context.Background(), store.TestSender(), h, &rsArgs)
	if pErr != nil {
		t.Fatal(pErr)
	}
	rsReply := reply.(*roachpb.ReverseScanResponse)
	if a, e := len(rsReply.Rows), 2; a != e {
		t.Errorf("expected %d rows; got %d", e, a)
	}
	expResumeSpan = &roachpb.Span{Key: roachpb.Key("a"), EndKey: roachpb.Key("a").Next()}
	if a, e := rsReply.ResumeSpan, expResumeSpan; !reflect.DeepEqual(a, e) {
		t.Errorf("expected resume span %s; got %s", e, a)
	}

	// Verify the timestamp cache has been set for "a".Next(), but not for "a".
	rTS, _ = store.tsCache.GetMaxRead(roachpb.Key("a").Next(), nil)
	if a, e := rTS, makeTS(t2.Nanoseconds(), 0); a != e {
		t.Errorf("expected timestamp cache for \"a\".Next() set to %s; got %s", e, a)
	}
	rTS, _ = store.tsCache.GetMaxRead(roachpb.Key("a"), nil)
	if a, lt := rTS, makeTS(t2.Nanoseconds(), 0); !a.Less(lt) {
		t.Errorf("expected timestamp cache for \"a\" set less than %s; got %s", lt, a)
	}
}

// TestStoreScanIntents verifies that a scan across 10 intents resolves
// them in one fell swoop using both consistent and inconsistent reads.
func TestStoreScanIntents(t *testing.T) {
	defer leaktest.AfterTest(t)()

	cfg := TestStoreConfig(nil)
	var count int32
	countPtr := &count

	cfg.TestingKnobs.EvalKnobs.TestingEvalFilter =
		func(filterArgs storagebase.FilterArgs) *roachpb.Error {
			if req, ok := filterArgs.Req.(*roachpb.ScanRequest); ok {
				// Avoid counting scan requests not generated by this test, e.g. those
				// generated by periodic gossips.
				if bytes.HasPrefix(req.Key, []byte(t.Name())) {
					atomic.AddInt32(countPtr, 1)
				}
			}
			return nil
		}
	stopper := stop.NewStopper()
	defer stopper.Stop(context.TODO())
	store := createTestStoreWithConfig(t, stopper, testStoreOpts{createSystemRanges: true}, &cfg)

	testCases := []struct {
		consistent bool
		canPush    bool  // can the txn be pushed?
		expFinish  bool  // do we expect the scan to finish?
		expCount   int32 // how many times do we expect to scan?
	}{
		// Consistent which can push will make two loops.
		{true, true, true, 2},
		// Consistent but can't push will backoff and retry and not finish.
		{true, false, false, -1},
		// Inconsistent and can push will make one loop, with async resolves.
		{false, true, true, 1},
		// Inconsistent and can't push will just read inconsistent (will read nils).
		{false, false, true, 1},
	}
	for i, test := range testCases {
		// The command filter just counts the number of scan requests which are
		// submitted to the range.
		atomic.StoreInt32(countPtr, 0)

		// Lay down 10 intents to scan over.
		var txn *roachpb.Transaction
		keys := []roachpb.Key{}
		for j := 0; j < 10; j++ {
			key := roachpb.Key(fmt.Sprintf("%s%d-%02d", t.Name(), i, j))
			keys = append(keys, key)
			if txn == nil {
				priority := roachpb.UserPriority(1)
				if test.canPush {
					priority = roachpb.MinUserPriority
				}
				txn = newTransaction(fmt.Sprintf("test-%d", i), key, priority, store.cfg.Clock)
			}
			args := putArgs(key, []byte(fmt.Sprintf("value%02d", j)))
			assignSeqNumsForReqs(txn, &args)
			if _, pErr := client.SendWrappedWith(context.Background(), store.TestSender(), roachpb.Header{Txn: txn}, &args); pErr != nil {
				t.Fatal(pErr)
			}
		}

		// Scan the range and verify count. Do this in a goroutine in case
		// it isn't expected to finish.
		sArgs := scanArgs(keys[0], keys[9].Next())
		var sReply *roachpb.ScanResponse
		ts := store.Clock().Now()
		consistency := roachpb.CONSISTENT
		if !test.consistent {
			consistency = roachpb.INCONSISTENT
		}
		errChan := make(chan *roachpb.Error, 1)
		go func() {
			reply, pErr := client.SendWrappedWith(context.Background(), store.TestSender(), roachpb.Header{
				Timestamp:       ts,
				ReadConsistency: consistency,
			}, &sArgs)
			if pErr == nil {
				sReply = reply.(*roachpb.ScanResponse)
			}
			errChan <- pErr
		}()

		wait := 1 * time.Second
		if !test.expFinish {
			wait = 10 * time.Millisecond
		}
		select {
		case pErr := <-errChan:
			if pErr != nil {
				t.Fatal(pErr)
			}
			if len(sReply.Rows) != 0 {
				t.Errorf("expected empty scan result; got %+v", sReply.Rows)
			}
			if countVal := atomic.LoadInt32(countPtr); countVal != test.expCount {
				t.Errorf("%d: expected scan count %d; got %d", i, test.expCount, countVal)
			}
		case <-time.After(wait):
			if test.expFinish {
				t.Errorf("%d: scan failed to finish after %s", i, wait)
			} else {
				// Commit the unpushable txn so the read can finish.
				etArgs, h := endTxnArgs(txn, true)
				for _, key := range keys {
					etArgs.IntentSpans = append(etArgs.IntentSpans, roachpb.Span{Key: key})
				}
				assignSeqNumsForReqs(txn, &etArgs)
				if _, pErr := client.SendWrappedWith(context.Background(), store.TestSender(), h, &etArgs); pErr != nil {
					t.Fatal(pErr)
				}
				<-errChan
			}
		}
	}
}

// TestStoreScanInconsistentResolvesIntents lays down 10 intents,
// commits the txn without resolving intents, then does repeated
// inconsistent reads until the data shows up, showing that the
// inconsistent reads are triggering intent resolution.
func TestStoreScanInconsistentResolvesIntents(t *testing.T) {
	defer leaktest.AfterTest(t)()
	// This test relies on having a committed Txn record and open intents on
	// the same Range. This only works with auto-gc turned off; alternatively
	// the test could move to splitting its underlying Range.
	defer setTxnAutoGC(false)()
	var intercept atomic.Value
	intercept.Store(true)
	cfg := TestStoreConfig(nil)
	cfg.TestingKnobs.EvalKnobs.TestingEvalFilter =
		func(filterArgs storagebase.FilterArgs) *roachpb.Error {
			_, ok := filterArgs.Req.(*roachpb.ResolveIntentRequest)
			if ok && intercept.Load().(bool) {
				return roachpb.NewErrorWithTxn(errors.Errorf("boom"), filterArgs.Hdr.Txn)
			}
			return nil
		}
	stopper := stop.NewStopper()
	defer stopper.Stop(context.TODO())
	store := createTestStoreWithConfig(t, stopper, testStoreOpts{createSystemRanges: true}, &cfg)

	// Lay down 10 intents to scan over.
	txn := newTransaction("test", roachpb.Key("foo"), 1, store.cfg.Clock)
	keys := []roachpb.Key{}
	for j := 0; j < 10; j++ {
		key := roachpb.Key(fmt.Sprintf("key%02d", j))
		keys = append(keys, key)
		args := putArgs(key, []byte(fmt.Sprintf("value%02d", j)))
		assignSeqNumsForReqs(txn, &args)
		if _, pErr := client.SendWrappedWith(context.Background(), store.TestSender(), roachpb.Header{Txn: txn}, &args); pErr != nil {
			t.Fatal(pErr)
		}
	}

	// Now, commit txn without resolving intents. If we hadn't disabled auto-gc
	// of Txn entries in this test, the Txn entry would be removed and later
	// attempts to resolve the intents would fail.
	etArgs, h := endTxnArgs(txn, true)
	assignSeqNumsForReqs(txn, &etArgs)
	if _, pErr := client.SendWrappedWith(context.Background(), store.TestSender(), h, &etArgs); pErr != nil {
		t.Fatal(pErr)
	}

	intercept.Store(false) // allow async intent resolution

	// Scan the range repeatedly until we've verified count.
	sArgs := scanArgs(keys[0], keys[9].Next())
	testutils.SucceedsSoon(t, func() error {
		if reply, pErr := client.SendWrappedWith(context.Background(), store.TestSender(), roachpb.Header{
			ReadConsistency: roachpb.INCONSISTENT,
		}, &sArgs); pErr != nil {
			return pErr.GoError()
		} else if sReply := reply.(*roachpb.ScanResponse); len(sReply.Rows) != 10 {
			return errors.Errorf("could not read rows as expected")
		}
		return nil
	})
}

// TestStoreScanIntentsFromTwoTxns lays down two intents from two
// different transactions. The clock is next moved forward, causing
// the transaction to expire. The intents are then scanned
// consistently, which triggers a push of both transactions and then
// resolution of the intents, allowing the scan to complete.
func TestStoreScanIntentsFromTwoTxns(t *testing.T) {
	defer leaktest.AfterTest(t)()
	stopper := stop.NewStopper()
	defer stopper.Stop(context.TODO())
	store, manualClock := createTestStore(t, testStoreOpts{createSystemRanges: true}, stopper)

	// Lay down two intents from two txns to scan over.
	key1 := roachpb.Key("bar")
	txn1 := newTransaction("test1", key1, 1, store.cfg.Clock)
	args := putArgs(key1, []byte("value1"))
	assignSeqNumsForReqs(txn1, &args)
	if _, pErr := client.SendWrappedWith(context.Background(), store.TestSender(), roachpb.Header{Txn: txn1}, &args); pErr != nil {
		t.Fatal(pErr)
	}

	key2 := roachpb.Key("foo")
	txn2 := newTransaction("test2", key2, 1, store.cfg.Clock)
	args = putArgs(key2, []byte("value2"))
	assignSeqNumsForReqs(txn2, &args)
	if _, pErr := client.SendWrappedWith(context.Background(), store.TestSender(), roachpb.Header{Txn: txn2}, &args); pErr != nil {
		t.Fatal(pErr)
	}

	// Now, expire the transactions by moving the clock forward. This will
	// result in the subsequent scan operation pushing both transactions
	// in a single batch.
	manualClock.Increment(txnwait.TxnLivenessThreshold.Nanoseconds() + 1)

	// Scan the range and verify empty result (expired txn is aborted,
	// cleaning up intents).
	sArgs := scanArgs(key1, key2.Next())
	if reply, pErr := client.SendWrappedWith(context.Background(), store.TestSender(), roachpb.Header{}, &sArgs); pErr != nil {
		t.Fatal(pErr)
	} else if sReply := reply.(*roachpb.ScanResponse); len(sReply.Rows) != 0 {
		t.Errorf("expected empty result; got %+v", sReply.Rows)
	}
}

// TestStoreScanMultipleIntents lays down ten intents from a single
// transaction. The clock is then moved forward such that the txn is
// expired and the intents are scanned INCONSISTENTly. Verify that all
// ten intents are resolved from a single INCONSISTENT scan.
func TestStoreScanMultipleIntents(t *testing.T) {
	defer leaktest.AfterTest(t)()

	var resolveCount int32
	manual := hlc.NewManualClock(123)
	cfg := TestStoreConfig(hlc.NewClock(manual.UnixNano, time.Nanosecond))
	cfg.TestingKnobs.EvalKnobs.TestingEvalFilter =
		func(filterArgs storagebase.FilterArgs) *roachpb.Error {
			if _, ok := filterArgs.Req.(*roachpb.ResolveIntentRequest); ok {
				atomic.AddInt32(&resolveCount, 1)
			}
			return nil
		}
	stopper := stop.NewStopper()
	defer stopper.Stop(context.TODO())
	store := createTestStoreWithConfig(t, stopper, testStoreOpts{createSystemRanges: true}, &cfg)

	// Lay down ten intents from a single txn.
	key1 := roachpb.Key("key00")
	key10 := roachpb.Key("key09")
	txn := newTransaction("test", key1, 1, store.cfg.Clock)
	ba := roachpb.BatchRequest{}
	for i := 0; i < 10; i++ {
		pArgs := putArgs(roachpb.Key(fmt.Sprintf("key%02d", i)), []byte("value"))
		ba.Add(&pArgs)
		assignSeqNumsForReqs(txn, &pArgs)
	}
	ba.Header = roachpb.Header{Txn: txn}
	if _, pErr := store.TestSender().Send(context.Background(), ba); pErr != nil {
		t.Fatal(pErr)
	}

	// Now, expire the transactions by moving the clock forward. This will
	// result in the subsequent scan operation pushing both transactions
	// in a single batch.
	manual.Increment(txnwait.TxnLivenessThreshold.Nanoseconds() + 1)

	// Query the range with a single scan, which should cause all intents
	// to be resolved.
	sArgs := scanArgs(key1, key10.Next())
	if _, pErr := client.SendWrapped(context.Background(), store.TestSender(), &sArgs); pErr != nil {
		t.Fatal(pErr)
	}

	// Verify all ten intents are resolved from the single inconsistent scan.
	testutils.SucceedsSoon(t, func() error {
		if a, e := atomic.LoadInt32(&resolveCount), int32(10); a != e {
			return fmt.Errorf("expected %d; got %d resolves", e, a)
		}
		return nil
	})
}

// TestStoreBadRequests verifies that Send returns errors for
// bad requests that do not pass key verification.
//
// TODO(kkaneda): Add more test cases.
func TestStoreBadRequests(t *testing.T) {
	defer leaktest.AfterTest(t)()
	stopper := stop.NewStopper()
	defer stopper.Stop(context.TODO())
	store, _ := createTestStore(t, testStoreOpts{createSystemRanges: true}, stopper)

	txn := newTransaction("test", roachpb.Key("a"), 1 /* priority */, store.cfg.Clock)

	args1 := getArgs(roachpb.Key("a"))
	args1.EndKey = roachpb.Key("b")

	args2 := getArgs(roachpb.RKeyMax)

	args3 := scanArgs(roachpb.Key("a"), roachpb.Key("a"))
	args4 := scanArgs(roachpb.Key("b"), roachpb.Key("a"))

	args5 := scanArgs(roachpb.RKeyMin, roachpb.Key("a"))
	args6 := scanArgs(keys.RangeDescriptorKey(roachpb.RKey(keys.MinKey)), roachpb.Key("a"))

	tArgs0, _ := endTxnArgs(txn, false /* commit */)
	tArgs1, _ := heartbeatArgs(txn, hlc.Timestamp{})

	tArgs2, tHeader2 := endTxnArgs(txn, false /* commit */)
	tHeader2.Txn.Key = roachpb.Key(tHeader2.Txn.Key).Next()

	tArgs3, tHeader3 := heartbeatArgs(txn, hlc.Timestamp{})
	tHeader3.Txn.Key = roachpb.Key(tHeader3.Txn.Key).Next()

	tArgs4 := pushTxnArgs(txn, txn, roachpb.PUSH_ABORT)
	tArgs4.PusheeTxn.Key = roachpb.Key(txn.Key).Next()

	testCases := []struct {
		args   roachpb.Request
		header *roachpb.Header
		err    string
	}{
		// EndKey for non-Range is invalid.
		{&args1, nil, "should not be specified"},
		// Start key must be less than KeyMax.
		{&args2, nil, "must be less than"},
		// End key must be greater than start.
		{&args3, nil, "must be greater than"},
		{&args4, nil, "must be greater than"},
		// Can't range from local to global.
		{&args5, nil, "must be greater than LocalMax"},
		{&args6, nil, "is range-local, but"},
		// Txn must be specified in Header.
		{&tArgs0, nil, "no transaction specified"},
		{&tArgs1, nil, "no transaction specified"},
		// Txn key must be same as the request key.
		{&tArgs2, &tHeader2, "request key .* should match txn key .*"},
		{&tArgs3, &tHeader3, "request key .* should match txn key .*"},
		{&tArgs4, nil, "request key .* should match pushee"},
	}
	for i, test := range testCases {
		if test.header == nil {
			test.header = &roachpb.Header{}
		}
		if test.header.Txn != nil {
			assignSeqNumsForReqs(test.header.Txn, test.args)
		}
		if _, pErr := client.SendWrappedWith(context.Background(), store.TestSender(), *test.header, test.args); !testutils.IsPError(pErr, test.err) {
			t.Errorf("%d expected error %q, got error %v", i, test.err, pErr)
		}
	}
}

// fakeRangeQueue implements the rangeQueue interface and
// records which range is passed to MaybeRemove.
type fakeRangeQueue struct {
	maybeRemovedRngs chan roachpb.RangeID
}

func (fq *fakeRangeQueue) Start(_ *stop.Stopper) {
	// Do nothing
}

func (fq *fakeRangeQueue) MaybeAddAsync(context.Context, replicaInQueue, hlc.Timestamp) {
	// Do nothing
}

func (fq *fakeRangeQueue) MaybeRemove(rangeID roachpb.RangeID) {
	fq.maybeRemovedRngs <- rangeID
}

func (fq *fakeRangeQueue) Name() string {
	return "fakeRangeQueue"
}

func (fq *fakeRangeQueue) NeedsLease() bool {
	return false
}

// TestMaybeRemove tests that MaybeRemove is called when a range is removed.
func TestMaybeRemove(t *testing.T) {
	defer leaktest.AfterTest(t)()
	cfg := TestStoreConfig(nil)
	stopper := stop.NewStopper()
	defer stopper.Stop(context.TODO())
	store := createTestStoreWithoutStart(t, stopper, testStoreOpts{createSystemRanges: true}, &cfg)

	// Add a queue to the scanner before starting the store and running the scanner.
	// This is necessary to avoid data race.
	fq := &fakeRangeQueue{
		maybeRemovedRngs: make(chan roachpb.RangeID),
	}
	store.scanner.AddQueues(fq)

	if err := store.Start(context.Background(), stopper); err != nil {
		t.Fatal(err)
	}
	store.WaitForInit()

	repl, err := store.GetReplica(1)
	if err != nil {
		t.Error(err)
	}
	if err := store.RemoveReplica(context.Background(), repl, repl.Desc().NextReplicaID, RemoveOptions{
		DestroyData: true,
	}); err != nil {
		t.Error(err)
	}
	// MaybeRemove is called.
	removedRng := <-fq.maybeRemovedRngs
	if removedRng != repl.RangeID {
		t.Errorf("Unexpected removed range %v", removedRng)
	}
}

func TestStoreGCThreshold(t *testing.T) {
	defer leaktest.AfterTest(t)()
	tc := testContext{}
	stopper := stop.NewStopper()
	defer stopper.Stop(context.TODO())
	tc.Start(t, stopper)
	store := tc.store

	assertThreshold := func(ts hlc.Timestamp) {
		repl, err := store.GetReplica(1)
		if err != nil {
			t.Fatal(err)
		}
		repl.mu.Lock()
		gcThreshold := *repl.mu.state.GCThreshold
		pgcThreshold, err := repl.mu.stateLoader.LoadGCThreshold(context.Background(), store.Engine())
		repl.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		if gcThreshold != *pgcThreshold {
			t.Fatalf("persisted != in-memory threshold: %s vs %s", pgcThreshold, gcThreshold)
		}
		if *pgcThreshold != ts {
			t.Fatalf("expected timestamp %s, got %s", ts, pgcThreshold)
		}
	}

	// Threshold should start at zero.
	assertThreshold(hlc.Timestamp{})

	threshold := hlc.Timestamp{
		WallTime: 2E9,
	}

	gcr := roachpb.GCRequest{
		// Bogus span to make it a valid request.
		RequestHeader: roachpb.RequestHeader{
			Key:    roachpb.Key("a"),
			EndKey: roachpb.Key("b"),
		},
		Threshold: threshold,
	}
	if _, pErr := tc.SendWrappedWith(roachpb.Header{RangeID: 1}, &gcr); pErr != nil {
		t.Fatal(pErr)
	}

	assertThreshold(threshold)
}

func TestStoreRangePlaceholders(t *testing.T) {
	defer leaktest.AfterTest(t)()
	tc := testContext{}
	stopper := stop.NewStopper()
	ctx := context.TODO()
	defer stopper.Stop(ctx)
	tc.Start(t, stopper)
	s := tc.store

	s.mu.Lock()
	numPlaceholders := len(s.mu.replicaPlaceholders)
	s.mu.Unlock()

	if numPlaceholders != 0 {
		t.Fatal("new store should have zero replica placeholders")
	}

	// Clobber the existing range so we can test non-overlapping placeholders.
	repl1, err := s.GetReplica(1)
	if err != nil {
		t.Error(err)
	}
	if err := s.RemoveReplica(ctx, repl1, repl1.Desc().NextReplicaID, RemoveOptions{
		DestroyData: true,
	}); err != nil {
		t.Error(err)
	}

	repID := roachpb.RangeID(2)
	rep := createReplica(s, repID, roachpb.RKeyMin, roachpb.RKey("c"))
	if err := s.AddReplica(rep); err != nil {
		t.Fatal(err)
	}

	placeholder1 := &ReplicaPlaceholder{
		rangeDesc: roachpb.RangeDescriptor{
			RangeID:  roachpb.RangeID(7),
			StartKey: roachpb.RKey("c"),
			EndKey:   roachpb.RKey("d"),
		},
	}
	placeholder2 := &ReplicaPlaceholder{
		rangeDesc: roachpb.RangeDescriptor{
			RangeID:  roachpb.RangeID(8),
			StartKey: roachpb.RKey("d"),
			EndKey:   roachpb.RKeyMax,
		},
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Test that simple insertion works.
	if err := s.addPlaceholderLocked(placeholder1); err != nil {
		t.Fatalf("could not add placeholder to empty store, got %s", err)
	}
	if err := s.addPlaceholderLocked(placeholder2); err != nil {
		t.Fatalf("could not add non-overlapping placeholder, got %s", err)
	}

	// Test that simple deletion works.
	if !s.removePlaceholderLocked(ctx, placeholder1.rangeDesc.RangeID) {
		t.Fatalf("could not remove placeholder that was present")
	}

	// Test cannot double insert the same placeholder.
	if err := s.addPlaceholderLocked(placeholder1); err != nil {
		t.Fatalf("could not re-add placeholder after removal, got %s", err)
	}
	if err := s.addPlaceholderLocked(placeholder1); !testutils.IsError(err, ".*overlaps with existing KeyRange") {
		t.Fatalf("should not be able to add ReplicaPlaceholder for the same key twice, got: %+v", err)
	}

	// Test cannot double delete a placeholder.
	if !s.removePlaceholderLocked(ctx, placeholder1.rangeDesc.RangeID) {
		t.Fatalf("could not remove placeholder that was present")
	}
	if s.removePlaceholderLocked(ctx, placeholder1.rangeDesc.RangeID) {
		t.Fatalf("successfully removed placeholder that was not present")
	}

	// This placeholder overlaps with an existing replica.
	placeholder1 = &ReplicaPlaceholder{
		rangeDesc: roachpb.RangeDescriptor{
			RangeID:  repID,
			StartKey: roachpb.RKeyMin,
			EndKey:   roachpb.RKey("c"),
		},
	}

	// Test that placeholder cannot clobber existing replica.
	if err := s.addPlaceholderLocked(placeholder1); !testutils.IsError(err, ".*overlaps with existing KeyRange") {
		t.Fatalf("should not be able to add ReplicaPlaceholder when Replica already exists, got: %+v", err)
	}

	// Test that Placeholder deletion doesn't delete replicas.
	if s.removePlaceholderLocked(ctx, repID) {
		t.Fatalf("should not be able to process removeReplicaPlaceholder for a RangeID where a Replica exists")
	}
}

// Test that we remove snapshot placeholders on error conditions.
func TestStoreRemovePlaceholderOnError(t *testing.T) {
	defer leaktest.AfterTest(t)()
	tc := testContext{}
	stopper := stop.NewStopper()
	defer stopper.Stop(context.TODO())
	tc.Start(t, stopper)
	s := tc.store
	ctx := context.Background()

	// Clobber the existing range so we can test nonoverlapping placeholders.
	repl1, err := s.GetReplica(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveReplica(context.Background(), repl1, repl1.Desc().NextReplicaID, RemoveOptions{
		DestroyData: true,
	}); err != nil {
		t.Fatal(err)
	}

	// Generate a minimal fake snapshot.
	snapData := &roachpb.RaftSnapshotData{}
	data, err := protoutil.Marshal(snapData)
	if err != nil {
		t.Fatal(err)
	}

	// Wrap the snapshot in a minimal header. The request will error because the
	// replica tombstone for the range requires that a new replica have an ID
	// greater than 1.
	snapHeader := &SnapshotRequest_Header{
		State: storagepb.ReplicaState{Desc: repl1.Desc()},
		RaftMessageRequest: RaftMessageRequest{
			RangeID: 1,
			ToReplica: roachpb.ReplicaDescriptor{
				NodeID:    1,
				StoreID:   1,
				ReplicaID: 0,
			},
			FromReplica: roachpb.ReplicaDescriptor{
				NodeID:    2,
				StoreID:   2,
				ReplicaID: 2,
			},
			Message: raftpb.Message{
				Type: raftpb.MsgSnap,
				Snapshot: raftpb.Snapshot{
					Data: data,
				},
			},
		},
	}
	const expected = "preemptive snapshot from term 0 received"
	if err := s.processPreemptiveSnapshotRequest(ctx, snapHeader,
		IncomingSnapshot{
			SnapUUID: uuid.MakeV4(),
			State:    &storagepb.ReplicaState{Desc: repl1.Desc()},
		}); !testutils.IsPError(err, expected) {
		t.Fatalf("expected %s, but found %v", expected, err)
	}

	s.mu.Lock()
	numPlaceholders := len(s.mu.replicaPlaceholders)
	s.mu.Unlock()

	if numPlaceholders != 0 {
		t.Fatalf("expected 0 placeholders, but found %d", numPlaceholders)
	}
	if n := atomic.LoadInt32(&s.counts.removedPlaceholders); n != 1 {
		t.Fatalf("expected 1 removed placeholder, but found %d", n)
	}
}

// Test that we remove snapshot placeholders when raft ignores the
// snapshot. This is testing the removal of placeholder after handleRaftReady
// processing for an uninitialized Replica.
func TestStoreRemovePlaceholderOnRaftIgnored(t *testing.T) {
	defer leaktest.AfterTest(t)()
	tc := testContext{}
	stopper := stop.NewStopper()
	defer stopper.Stop(context.TODO())
	tc.Start(t, stopper)
	s := tc.store
	ctx := context.Background()

	// Clobber the existing range so we can test nonoverlapping placeholders.
	repl1, err := s.GetReplica(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveReplica(context.Background(), repl1, repl1.Desc().NextReplicaID, RemoveOptions{
		DestroyData: true,
	}); err != nil {
		t.Fatal(err)
	}

	cv := s.ClusterSettings().Version.Version().Version
	if _, err := stateloader.WriteInitialState(
		ctx, s.Engine(), enginepb.MVCCStats{}, *repl1.Desc(), roachpb.Lease{},
		hlc.Timestamp{}, cv, stateloader.TruncatedStateUnreplicated,
	); err != nil {
		t.Fatal(err)
	}

	// Generate a minimal fake snapshot.
	snapData := &roachpb.RaftSnapshotData{}
	data, err := protoutil.Marshal(snapData)
	if err != nil {
		t.Fatal(err)
	}

	// Wrap the snapshot in a minimal header. The request will be dropped
	// because the Raft log index and term are less than the hard state written
	// above.
	req := &SnapshotRequest_Header{
		State: storagepb.ReplicaState{Desc: repl1.Desc()},
		RaftMessageRequest: RaftMessageRequest{
			RangeID: 1,
			ToReplica: roachpb.ReplicaDescriptor{
				NodeID:    1,
				StoreID:   1,
				ReplicaID: 2,
			},
			FromReplica: roachpb.ReplicaDescriptor{
				NodeID:    2,
				StoreID:   2,
				ReplicaID: 2,
			},
			Message: raftpb.Message{
				Type: raftpb.MsgSnap,
				Snapshot: raftpb.Snapshot{
					Data: data,
					Metadata: raftpb.SnapshotMetadata{
						Index: 1,
						Term:  1,
					},
				},
			},
		},
	}
	if err := s.processRaftSnapshotRequest(ctx, req,
		IncomingSnapshot{
			SnapUUID: uuid.MakeV4(),
			State:    &storagepb.ReplicaState{Desc: repl1.Desc()},
		}); err != nil {
		t.Fatal(err)
	}

	testutils.SucceedsSoon(t, func() error {
		s.mu.Lock()
		numPlaceholders := len(s.mu.replicaPlaceholders)
		s.mu.Unlock()

		if numPlaceholders != 0 {
			return errors.Errorf("expected 0 placeholders, but found %d", numPlaceholders)
		}
		// The count of dropped placeholders is incremented after the placeholder
		// is removed (and while not holding Store.mu), so we need to perform the
		// check of the number of dropped placeholders in this retry loop.
		if n := atomic.LoadInt32(&s.counts.droppedPlaceholders); n != 1 {
			return errors.Errorf("expected 1 dropped placeholder, but found %d", n)
		}
		return nil
	})
}

// Test that we set proper tombstones for removed replicas and use the
// tombstone to reject attempts to create a replica with a lesser ID.
func TestRemovedReplicaTombstone(t *testing.T) {
	defer leaktest.AfterTest(t)()

	const rangeID = 1
	creatingReplica := roachpb.ReplicaDescriptor{
		NodeID:    2,
		StoreID:   2,
		ReplicaID: 2,
	}

	// All test cases assume that the starting replica ID is 1. This assumption
	// is enforced by a check within the test logic.
	testCases := []struct {
		setReplicaID      roachpb.ReplicaID // set the existing replica to this before removing it
		descNextReplicaID roachpb.ReplicaID // the descriptor's NextReplicaID during replica removal
		createReplicaID   roachpb.ReplicaID // try creating a replica at this ID
		expectCreated     bool
	}{
		{1, 2, 2, true},
		{1, 2, 1, false},
		{1, 2, 1, false},
		{1, 3, 1, false},
		{1, 3, 2, false},
		{1, 3, 3, true},
		{1, 99, 98, false},
		{1, 99, 99, true},
		{2, 2, 2, false},
		{2, 2, 3, true},
		{2, 2, 99, true},
		{98, 2, 98, false},
		{98, 2, 99, true},
	}
	for _, c := range testCases {
		t.Run("", func(t *testing.T) {
			tc := testContext{}
			stopper := stop.NewStopper()
			ctx := context.TODO()
			defer stopper.Stop(ctx)
			tc.Start(t, stopper)
			s := tc.store

			repl1, err := s.GetReplica(rangeID)
			if err != nil {
				t.Fatal(err)
			}
			repl1.mu.Lock()
			if repl1.mu.replicaID != 1 {
				repl1.mu.Unlock()
				t.Fatalf("test precondition not met; expected ReplicaID=1, got %d", repl1.mu.replicaID)
			}
			repl1.mu.Unlock()

			// Try to trigger a race where the replica ID gets increased during the GC
			// process by taking the store lock and inserting a short sleep to cause
			// the goroutine to start running the setReplicaID call.
			errChan := make(chan error)

			func() {
				repl1.raftMu.Lock()
				defer repl1.raftMu.Unlock()
				s.mu.Lock()
				defer s.mu.Unlock()
				repl1.mu.Lock()
				defer repl1.mu.Unlock()

				go func() {
					errChan <- s.RemoveReplica(ctx, repl1, c.descNextReplicaID, RemoveOptions{DestroyData: true})
				}()

				time.Sleep(1 * time.Millisecond)

				if err := repl1.setReplicaIDRaftMuLockedMuLocked(c.setReplicaID); err != nil {
					t.Fatal(err)
				}
			}()

			if err := <-errChan; testutils.IsError(err, "replica ID has changed") {
				// We didn't trigger the race, so just return success.
				return
			} else if err != nil {
				t.Fatal(err)
			}

			_, created, err := s.getOrCreateReplica(ctx, rangeID, c.createReplicaID, &creatingReplica)
			if created != c.expectCreated {
				t.Errorf("expected s.getOrCreateReplica(%d, %d, %v).created=%v, got %v",
					rangeID, c.createReplicaID, creatingReplica, c.expectCreated, created)
			}
			if !c.expectCreated && !testutils.IsError(err, "raft group deleted") {
				t.Errorf("expected s.getOrCreateReplica(%d, %d, %v).err='raft group deleted', got %v",
					rangeID, c.createReplicaID, creatingReplica, err)
			}
		})
	}
}

type fakeSnapshotStream struct {
	nextResp *SnapshotResponse
	nextErr  error
}

func (c fakeSnapshotStream) Recv() (*SnapshotResponse, error) {
	return c.nextResp, c.nextErr
}

func (c fakeSnapshotStream) Send(request *SnapshotRequest) error {
	return nil
}

type fakeStorePool struct {
	declinedThrottles int
	failedThrottles   int
}

func (sp *fakeStorePool) throttle(reason throttleReason, why string, toStoreID roachpb.StoreID) {
	switch reason {
	case throttleDeclined:
		sp.declinedThrottles++
	case throttleFailed:
		sp.failedThrottles++
	}
}

// TestSendSnapshotThrottling tests the store pool throttling behavior of
// store.sendSnapshot, ensuring that it properly updates the StorePool on
// various exceptional conditions and new capacity estimates.
func TestSendSnapshotThrottling(t *testing.T) {
	defer leaktest.AfterTest(t)()
	e := engine.NewInMem(roachpb.Attributes{}, 1<<10)
	defer e.Close()

	ctx := context.Background()
	var cfg base.RaftConfig
	cfg.SetDefaults()
	st := cluster.MakeTestingClusterSettings()

	header := SnapshotRequest_Header{
		CanDecline: true,
		State: storagepb.ReplicaState{
			Desc: &roachpb.RangeDescriptor{RangeID: 1},
		},
	}
	newBatch := e.NewBatch

	// Test that a failed Recv() causes a fail throttle
	{
		sp := &fakeStorePool{}
		expectedErr := errors.New("")
		c := fakeSnapshotStream{nil, expectedErr}
		err := sendSnapshot(ctx, &cfg, st, c, sp, header, nil, newBatch, nil)
		if sp.failedThrottles != 1 {
			t.Fatalf("expected 1 failed throttle, but found %d", sp.failedThrottles)
		}
		if err != expectedErr {
			t.Fatalf("expected error %s, but found %s", err, expectedErr)
		}
	}

	// Test that a declined snapshot causes a decline throttle.
	{
		sp := &fakeStorePool{}
		resp := &SnapshotResponse{
			Status: SnapshotResponse_DECLINED,
		}
		c := fakeSnapshotStream{resp, nil}
		err := sendSnapshot(ctx, &cfg, st, c, sp, header, nil, newBatch, nil)
		if sp.declinedThrottles != 1 {
			t.Fatalf("expected 1 declined throttle, but found %d", sp.declinedThrottles)
		}
		if err == nil {
			t.Fatalf("expected error, found nil")
		}
	}

	// Test that a declined but required snapshot causes a fail throttle.
	{
		sp := &fakeStorePool{}
		header.CanDecline = false
		resp := &SnapshotResponse{
			Status: SnapshotResponse_DECLINED,
		}
		c := fakeSnapshotStream{resp, nil}
		err := sendSnapshot(ctx, &cfg, st, c, sp, header, nil, newBatch, nil)
		if sp.failedThrottles != 1 {
			t.Fatalf("expected 1 failed throttle, but found %d", sp.failedThrottles)
		}
		if err == nil {
			t.Fatalf("expected error, found nil")
		}
	}

	// Test that an errored snapshot causes a fail throttle.
	{
		sp := &fakeStorePool{}
		resp := &SnapshotResponse{
			Status: SnapshotResponse_ERROR,
		}
		c := fakeSnapshotStream{resp, nil}
		err := sendSnapshot(ctx, &cfg, st, c, sp, header, nil, newBatch, nil)
		if sp.failedThrottles != 1 {
			t.Fatalf("expected 1 failed throttle, but found %d", sp.failedThrottles)
		}
		if err == nil {
			t.Fatalf("expected error, found nil")
		}
	}
}

func TestReserveSnapshotThrottling(t *testing.T) {
	defer leaktest.AfterTest(t)()

	stopper := stop.NewStopper()
	defer stopper.Stop(context.TODO())
	tc := testContext{}
	tc.Start(t, stopper)
	s := tc.store

	ctx := context.Background()

	cleanupNonEmpty1, rejectionMsg, err := s.reserveSnapshot(ctx, &SnapshotRequest_Header{
		RangeSize: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rejectionMsg != "" {
		t.Fatalf("expected no rejection message, got %q", rejectionMsg)
	}
	if n := s.ReservationCount(); n != 1 {
		t.Fatalf("expected 1 reservation, but found %d", n)
	}

	// Ensure we allow a concurrent empty snapshot.
	cleanupEmpty, rejectionMsg, err := s.reserveSnapshot(ctx, &SnapshotRequest_Header{})
	if err != nil {
		t.Fatal(err)
	}
	if rejectionMsg != "" {
		t.Fatalf("expected no rejection message, got %q", rejectionMsg)
	}
	// Empty snapshots are not throttled and so do not increase the reservation
	// count.
	if n := s.ReservationCount(); n != 1 {
		t.Fatalf("expected 1 reservation, but found %d", n)
	}
	cleanupEmpty()

	// Verify that a declinable snapshot will be declined if another is in
	// progress.
	cleanupNonEmpty2, rejectionMsg, err := s.reserveSnapshot(ctx, &SnapshotRequest_Header{
		RangeSize:  1,
		CanDecline: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rejectionMsg != snapshotApplySemBusyMsg {
		t.Fatalf("expected rejection message %q, got %q", snapshotApplySemBusyMsg, rejectionMsg)
	}
	if cleanupNonEmpty2 != nil {
		t.Fatalf("got unexpected non-nil cleanup method")
	}
	if n := s.ReservationCount(); n != 1 {
		t.Fatalf("expected 1 reservation, but found %d", n)
	}

	// Verify we block concurrent snapshots by spawning a goroutine which will
	// execute the cleanup after a short delay but only if another snapshot was
	// not allowed through.
	var boom int32
	go func() {
		time.Sleep(20 * time.Millisecond)
		if atomic.LoadInt32(&boom) == 0 {
			cleanupNonEmpty1()
		}
	}()

	cleanupNonEmpty3, rejectionMsg, err := s.reserveSnapshot(ctx, &SnapshotRequest_Header{
		RangeSize: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rejectionMsg != "" {
		t.Fatalf("expected no rejection message, got %q", rejectionMsg)
	}
	atomic.StoreInt32(&boom, 1)
	cleanupNonEmpty3()

	if n := s.ReservationCount(); n != 0 {
		t.Fatalf("expected 0 reservations, but found %d", n)
	}
}

// TestReserveSnapshotFullnessLimit verifies that snapshots are rejected when
// the recipient store's disk is near full.
func TestReserveSnapshotFullnessLimit(t *testing.T) {
	defer leaktest.AfterTest(t)()

	stopper := stop.NewStopper()
	defer stopper.Stop(context.TODO())
	tc := testContext{}
	tc.Start(t, stopper)
	s := tc.store

	ctx := context.Background()

	desc, err := s.Descriptor(false /* useCached */)
	if err != nil {
		t.Fatal(err)
	}
	desc.Capacity.Available = 1
	desc.Capacity.Used = desc.Capacity.Capacity - desc.Capacity.Available

	s.cfg.StorePool.detailsMu.Lock()
	s.cfg.StorePool.getStoreDetailLocked(desc.StoreID).desc = desc
	s.cfg.StorePool.detailsMu.Unlock()

	// A declinable snapshot to a nearly full store should be rejected.
	cleanupRejected, rejectionMsg, err := s.reserveSnapshot(ctx, &SnapshotRequest_Header{
		RangeSize:  1,
		CanDecline: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rejectionMsg != snapshotStoreTooFullMsg {
		t.Fatalf("expected rejection message %q, got %q", snapshotStoreTooFullMsg, rejectionMsg)
	}
	if cleanupRejected != nil {
		t.Fatalf("got unexpected non-nil cleanup method")
	}
	if n := s.ReservationCount(); n != 0 {
		t.Fatalf("expected 0 reservations, but found %d", n)
	}

	// But a non-declinable snapshot should be allowed.
	cleanupAccepted, rejectionMsg, err := s.reserveSnapshot(ctx, &SnapshotRequest_Header{
		RangeSize:  1,
		CanDecline: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rejectionMsg != "" {
		t.Fatalf("expected no rejection message, got %q", rejectionMsg)
	}
	if n := s.ReservationCount(); n != 1 {
		t.Fatalf("expected 1 reservation, but found %d", n)
	}
	cleanupAccepted()

	// Even if the store isn't mostly full, a range that's larger than the
	// available disk space should be rejected.
	desc.Capacity.Available = desc.Capacity.Capacity / 2
	desc.Capacity.Used = desc.Capacity.Capacity - desc.Capacity.Available
	s.cfg.StorePool.detailsMu.Lock()
	s.cfg.StorePool.getStoreDetailLocked(desc.StoreID).desc = desc
	s.cfg.StorePool.detailsMu.Unlock()

	// A declinable snapshot to a nearly full store should be rejected.
	cleanupRejected2, rejectionMsg, err := s.reserveSnapshot(ctx, &SnapshotRequest_Header{
		RangeSize:  desc.Capacity.Available + 1,
		CanDecline: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rejectionMsg != snapshotStoreTooFullMsg {
		t.Fatalf("expected rejection message %q, got %q", snapshotStoreTooFullMsg, rejectionMsg)
	}
	if cleanupRejected2 != nil {
		t.Fatalf("got unexpected non-nil cleanup method")
	}
	if n := s.ReservationCount(); n != 0 {
		t.Fatalf("expected 0 reservations, but found %d", n)
	}
}

func TestSnapshotRateLimit(t *testing.T) {
	defer leaktest.AfterTest(t)()

	testCases := []struct {
		priority      SnapshotRequest_Priority
		expectedLimit rate.Limit
		expectedErr   string
	}{
		{SnapshotRequest_UNKNOWN, 0, "unknown snapshot priority"},
		{SnapshotRequest_RECOVERY, 8 << 20, ""},
		{SnapshotRequest_REBALANCE, 8 << 20, ""},
	}
	for _, c := range testCases {
		t.Run(c.priority.String(), func(t *testing.T) {
			limit, err := snapshotRateLimit(cluster.MakeTestingClusterSettings(), c.priority)
			if !testutils.IsError(err, c.expectedErr) {
				t.Fatalf("expected \"%s\", but found %v", c.expectedErr, err)
			}
			if c.expectedLimit != limit {
				t.Fatalf("expected %v, but found %v", c.expectedLimit, limit)
			}
		})
	}
}

func BenchmarkStoreGetReplica(b *testing.B) {
	stopper := stop.NewStopper()
	defer stopper.Stop(context.TODO())
	store, _ := createTestStore(b, testStoreOpts{createSystemRanges: true}, stopper)

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, err := store.GetReplica(1)
			if err != nil {
				b.Fatal(err)
			}
		}
	})
}
