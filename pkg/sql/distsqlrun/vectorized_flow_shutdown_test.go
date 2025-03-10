// Copyright 2019 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package distsqlrun

import (
	"context"
	"fmt"
	"math"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/cockroachdb/cockroach/pkg/col/coldata"
	"github.com/cockroachdb/cockroach/pkg/col/coltypes"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/sql/distsqlpb"
	"github.com/cockroachdb/cockroach/pkg/sql/exec"
	"github.com/cockroachdb/cockroach/pkg/sql/exec/colrpc"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/cockroach/pkg/testutils"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/randutil"
	"github.com/cockroachdb/cockroach/pkg/util/stop"
	"github.com/cockroachdb/cockroach/pkg/util/syncutil"
	"github.com/cockroachdb/cockroach/pkg/util/uuid"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

type mockDialer struct {
	addr net.Addr
	mu   struct {
		syncutil.Mutex
		conn *grpc.ClientConn
	}
}

func (d *mockDialer) Dial(context.Context, roachpb.NodeID) (*grpc.ClientConn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.mu.conn != nil {
		return d.mu.conn, nil
	}
	var err error
	d.mu.conn, err = grpc.Dial(d.addr.String(), grpc.WithInsecure(), grpc.WithBlock())
	return d.mu.conn, err
}

// close must be called after the test.
func (d *mockDialer) close() {
	if err := d.mu.conn.Close(); err != nil {
		panic(err)
	}
}

type shutdownScenario struct {
	string
}

var (
	consumerDone      = shutdownScenario{"ConsumerDone"}
	consumerClosed    = shutdownScenario{"ConsumerClosed"}
	shutdownScenarios = []shutdownScenario{consumerDone, consumerClosed}
)

// TestVectorizedFlowShutdown tests that closing the materializer correctly
// closes all the infrastructure corresponding to the flow ending in that
// materializer. Namely:
// - on a remote node, it creates an exec.HashRouter with 3 outputs (with a
// corresponding to each Outbox) as well as 3 standalone Outboxes;
// - on a local node, it creates 6 exec.Inboxes that feed into an unordered
// synchronizer which then outputs all the data into a materializer.
// The resulting scheme looks as follows:
//
//            Remote Node             |                  Local Node
//                                    |
//             -> output -> Outbox -> | -> Inbox -> |
//            |                       |
// Hash Router -> output -> Outbox -> | -> Inbox -> |
//            |                       |
//             -> output -> Outbox -> | -> Inbox -> |
//                                    |              -> Synchronizer -> materializer
//                          Outbox -> | -> Inbox -> |
//                                    |
//                          Outbox -> | -> Inbox -> |
//                                    |
//                          Outbox -> | -> Inbox -> |
//
// Also, with 50% probability, another remote node with the chain of an Outbox
// and Inbox is placed between the synchronizer and materializer. The resulting
// scheme then looks as follows:
//
//            Remote Node             |            Another Remote Node             |         Local Node
//                                    |                                            |
//             -> output -> Outbox -> | -> Inbox ->                                |
//            |                       |             |                              |
// Hash Router -> output -> Outbox -> | -> Inbox ->                                |
//            |                       |             |                              |
//             -> output -> Outbox -> | -> Inbox ->                                |
//                                    |             | -> Synchronizer -> Outbox -> | -> Inbox -> materializer
//                          Outbox -> | -> Inbox ->                                |
//                                    |             |                              |
//                          Outbox -> | -> Inbox ->                                |
//                                    |             |                              |
//                          Outbox -> | -> Inbox ->                                |
//
// Remote nodes are simulated by having separate contexts and separate outbox
// registries.
//
// Additionally, all Outboxes have a single metadata source. In ConsumerDone
// shutdown scenario, we check that the metadata has been successfully
// propagated from all of the metadata sources.
func TestVectorizedFlowShutdown(t *testing.T) {
	defer leaktest.AfterTest(t)()

	stopper := stop.NewStopper()
	defer stopper.Stop(context.Background())
	_, mockServer, addr, err := distsqlpb.StartMockDistSQLServer(
		hlc.NewClock(hlc.UnixNano, time.Nanosecond), stopper, staticNodeID,
	)
	require.NoError(t, err)
	dialer := &mockDialer{addr: addr}
	defer dialer.close()

	for run := 0; run < 10; run++ {
		for _, shutdownOperation := range shutdownScenarios {
			t.Run(fmt.Sprintf("shutdownScenario=%s", shutdownOperation.string), func(t *testing.T) {
				ctxLocal := context.Background()
				st := cluster.MakeTestingClusterSettings()
				evalCtx := tree.MakeTestingEvalContext(st)
				defer evalCtx.Stop(ctxLocal)
				flowCtx := &FlowCtx{
					EvalCtx: &evalCtx,
					Cfg:     &ServerConfig{Settings: st},
				}

				rng, _ := randutil.NewPseudoRand()
				var (
					err             error
					wg              sync.WaitGroup
					typs            = []coltypes.T{coltypes.Int64}
					semtyps         = []types.T{*types.Int}
					hashRouterInput = exec.NewRandomDataOp(
						rng,
						exec.RandomDataOpArgs{
							DeterministicTyps: typs,
							// Set a high number of batches to ensure that the HashRouter is
							// very far from being finished when the flow is shut down.
							NumBatches: math.MaxInt64,
							Selection:  true,
						},
					)
					numHashRouterOutputs        = 3
					numInboxes                  = numHashRouterOutputs + 3
					inboxes                     = make([]*colrpc.Inbox, 0, numInboxes+1)
					handleStreamErrCh           = make([]chan error, numInboxes+1)
					synchronizerInputs          = make([]exec.Operator, 0, numInboxes)
					materializerMetadataSources = make([]distsqlpb.MetadataSource, 0, numInboxes+1)
					streamID                    = 0
					addAnotherRemote            = rng.Float64() < 0.5
				)

				hashRouter, hashRouterOutputs := exec.NewHashRouter(hashRouterInput, typs, []int{0}, numHashRouterOutputs)
				for i := 0; i < numInboxes; i++ {
					inbox, err := colrpc.NewInbox(typs)
					require.NoError(t, err)
					inboxes = append(inboxes, inbox)
					materializerMetadataSources = append(materializerMetadataSources, inbox)
					synchronizerInputs = append(synchronizerInputs, exec.Operator(inbox))
				}
				synchronizer := exec.NewUnorderedSynchronizer(synchronizerInputs, typs, &wg)
				flowID := distsqlpb.FlowID{UUID: uuid.MakeV4()}

				runOutboxInbox := func(
					ctx context.Context,
					cancelFn context.CancelFunc,
					outboxInput exec.Operator,
					inbox *colrpc.Inbox,
					id int,
					outboxMetadataSources []distsqlpb.MetadataSource,
				) {
					outbox, err := colrpc.NewOutbox(
						outboxInput,
						typs,
						append(outboxMetadataSources,
							distsqlpb.CallbackMetadataSource{
								DrainMetaCb: func(ctx context.Context) []distsqlpb.ProducerMetadata {
									return []distsqlpb.ProducerMetadata{{Err: errors.Errorf("%d", id)}}
								},
							},
						),
					)
					require.NoError(t, err)
					wg.Add(1)
					go func(id int) {
						outbox.Run(ctx, dialer, staticNodeID, flowID, distsqlpb.StreamID(id), cancelFn)
						wg.Done()
					}(id)

					require.NoError(t, err)
					serverStreamNotification := <-mockServer.InboundStreams
					serverStream := serverStreamNotification.Stream
					handleStreamErrCh[id] = make(chan error, 1)
					doneFn := func() { close(serverStreamNotification.Donec) }
					wg.Add(1)
					go func(id int, stream distsqlpb.DistSQL_FlowStreamServer, doneFn func()) {
						handleStreamErrCh[id] <- inbox.RunWithStream(stream.Context(), stream)
						doneFn()
						wg.Done()
					}(id, serverStream, doneFn)
				}

				ctxRemote, cancelRemote := context.WithCancel(context.Background())
				// Linter says there is a possibility of "context leak" because
				// cancelRemote variable may not be used, so we defer the call to it.
				// This does not change anything about the test since we're blocking on
				// the wait group.
				defer cancelRemote()

				wg.Add(1)
				go func() {
					hashRouter.Run(ctxRemote)
					wg.Done()
				}()
				for i := 0; i < numInboxes; i++ {
					var outboxMetadataSources []distsqlpb.MetadataSource
					if i < numHashRouterOutputs {
						if i == 0 {
							// Only one outbox should drain the hash router.
							outboxMetadataSources = append(outboxMetadataSources, hashRouter)
						}
						runOutboxInbox(ctxRemote, cancelRemote, hashRouterOutputs[i], inboxes[i], streamID, outboxMetadataSources)
					} else {
						batch := coldata.NewMemBatch(typs)
						batch.SetLength(coldata.BatchSize)
						runOutboxInbox(ctxRemote, cancelRemote, exec.NewRepeatableBatchSource(batch), inboxes[i], streamID, outboxMetadataSources)
					}
					streamID++
				}

				var materializerInput exec.Operator
				ctxAnotherRemote, cancelAnotherRemote := context.WithCancel(context.Background())
				if addAnotherRemote {
					// Add another "remote" node to the flow.
					inbox, err := colrpc.NewInbox(typs)
					require.NoError(t, err)
					inboxes = append(inboxes, inbox)
					runOutboxInbox(ctxAnotherRemote, cancelAnotherRemote, synchronizer, inbox, streamID, materializerMetadataSources)
					streamID++
					// There is now only a single Inbox on the "local" node which is the
					// only metadata source.
					materializerMetadataSources = []distsqlpb.MetadataSource{inbox}
					materializerInput = inbox
				} else {
					materializerInput = synchronizer
				}

				ctxLocal, cancelLocal := context.WithCancel(ctxLocal)
				materializer, err := newMaterializer(
					flowCtx,
					1, /* processorID */
					materializerInput,
					semtyps,
					&distsqlpb.PostProcessSpec{},
					nil, /* output */
					materializerMetadataSources,
					nil, /* outputStatsToTrace */
					func() context.CancelFunc { return cancelLocal },
				)
				require.NoError(t, err)
				materializer.Start(ctxLocal)

				for i := 0; i < 10; i++ {
					row, meta := materializer.Next()
					require.NotNil(t, row)
					require.Nil(t, meta)
				}
				switch shutdownOperation {
				case consumerDone:
					materializer.ConsumerDone()
					receivedMetaFromID := make([]bool, streamID)
					metaCount := 0
					for {
						row, meta := materializer.Next()
						require.Nil(t, row)
						if meta == nil {
							break
						}
						metaCount++
						require.NotNil(t, meta.Err)
						id, err := strconv.Atoi(meta.Err.Error())
						require.NoError(t, err)
						require.False(t, receivedMetaFromID[id])
						receivedMetaFromID[id] = true
					}
					require.Equal(t, streamID, metaCount, fmt.Sprintf("received metadata from Outbox %+v", receivedMetaFromID))
				case consumerClosed:
					materializer.ConsumerClosed()
				}

				// When Outboxes are setup through vectorizedFlowCreator, the latter
				// keeps track of how many outboxes are on the node. When the last one
				// exits (and if there is no materializer on that node),
				// vectorizedFlowCreator will cancel the flow context of the node. To
				// simulate this, we manually cancel contexts of both remote nodes.
				cancelRemote()
				cancelAnotherRemote()

				for i := range inboxes {
					err = <-handleStreamErrCh[i]
					// We either should get no error or a context cancellation error.
					if err != nil {
						require.True(t, testutils.IsError(err, "context canceled"), err)
					}
				}
				wg.Wait()
			})
		}
	}
}
