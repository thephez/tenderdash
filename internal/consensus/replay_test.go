package consensus

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"
	"time"

	"github.com/dashevo/dashd-go/btcjson"

	"github.com/gogo/protobuf/proto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	dbm "github.com/tendermint/tm-db"

	abciclient "github.com/tendermint/tendermint/abci/client"
	"github.com/tendermint/tendermint/abci/example/kvstore"
	abci "github.com/tendermint/tendermint/abci/types"
	"github.com/tendermint/tendermint/config"
	"github.com/tendermint/tendermint/crypto"
	cryptoenc "github.com/tendermint/tendermint/crypto/encoding"
	mempl "github.com/tendermint/tendermint/internal/mempool"
	"github.com/tendermint/tendermint/internal/proxy"
	sm "github.com/tendermint/tendermint/internal/state"
	sf "github.com/tendermint/tendermint/internal/state/test/factory"
	"github.com/tendermint/tendermint/internal/store"
	"github.com/tendermint/tendermint/libs/log"
	tmrand "github.com/tendermint/tendermint/libs/rand"
	"github.com/tendermint/tendermint/privval"
	tmstate "github.com/tendermint/tendermint/proto/tendermint/state"
	tmproto "github.com/tendermint/tendermint/proto/tendermint/types"
	"github.com/tendermint/tendermint/types"
)

// These tests ensure we can always recover from failure at any part of the consensus process.
// There are two general failure scenarios: failure during consensus, and failure while applying the block.
// Only the latter interacts with the app and store,
// but the former has to deal with restrictions on re-use of priv_validator keys.
// The `WAL Tests` are for failures during the consensus;
// the `Handshake Tests` are for failures in applying the block.
// With the help of the WAL, we can recover from it all!

//------------------------------------------------------------------------------------------
// WAL Tests

// TODO: It would be better to verify explicitly which states we can recover from without the wal
// and which ones we need the wal for - then we'd also be able to only flush the
// wal writer when we need to, instead of with every message.

func startNewStateAndWaitForBlock(t *testing.T, consensusReplayConfig *config.Config,
	lastBlockHeight int64, blockDB dbm.DB, stateStore sm.Store) {
	logger := log.TestingLogger()
	state, err := sm.MakeGenesisStateFromFile(consensusReplayConfig.GenesisFile())
	require.NoError(t, err)
	privValidator := loadPrivValidator(consensusReplayConfig)
	blockStore := store.NewBlockStore(dbm.NewMemDB())
	cs := newStateWithConfigAndBlockStore(
		consensusReplayConfig,
		state,
		privValidator,
		kvstore.NewApplication(),
		blockStore,
	)
	cs.SetLogger(logger)

	bytes, _ := ioutil.ReadFile(cs.config.WalFile())
	t.Logf("====== WAL: \n\r%X\n", bytes)

	err = cs.Start()
	require.NoError(t, err)
	defer func() {
		if err := cs.Stop(); err != nil {
			t.Error(err)
		}
	}()

	// This is just a signal that we haven't halted; its not something contained
	// in the WAL itself. Assuming the consensus state is running, replay of any
	// WAL, including the empty one, should eventually be followed by a new
	// block, or else something is wrong.
	newBlockSub, err := cs.eventBus.Subscribe(
		context.Background(),
		testSubscriber,
		types.EventQueryNewBlock,
	)
	require.NoError(t, err)
	select {
	case <-newBlockSub.Out():
	case <-newBlockSub.Canceled():
		t.Fatal("newBlockSub was canceled")
	case <-time.After(240 * time.Second):
		t.Fatal("Timed out waiting for new block (see trace above)")
	}
}

func sendTxs(ctx context.Context, cs *State) {
	for i := 0; i < 256; i++ {
		select {
		case <-ctx.Done():
			return
		default:
			tx := []byte{byte(i)}
			if err := assertMempool(cs.txNotifier).CheckTx(context.Background(), tx, nil, mempl.TxInfo{}); err != nil {
				panic(err)
			}
			i++
		}
	}
}

// TestWALCrash uses crashing WAL to test we can recover from any WAL failure.
func TestWALCrash(t *testing.T) {
	testCases := []struct {
		name         string
		initFn       func(dbm.DB, *State, context.Context)
		heightToStop int64
	}{
		{"empty block",
			func(stateDB dbm.DB, cs *State, ctx context.Context) {},
			1},
		{"many non-empty blocks",
			func(stateDB dbm.DB, cs *State, ctx context.Context) {
				go sendTxs(ctx, cs)
			},
			3},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			consensusReplayConfig, err := ResetConfig(tc.name)
			require.NoError(t, err)
			crashWALandCheckLiveness(t, consensusReplayConfig, tc.initFn, tc.heightToStop)
		})
	}
}

func crashWALandCheckLiveness(t *testing.T, consensusReplayConfig *config.Config,
	initFn func(dbm.DB, *State, context.Context), heightToStop int64) {
	walPanicked := make(chan error)
	crashingWal := &crashingWAL{panicCh: walPanicked, heightToStop: heightToStop}

	i := 1
LOOP:
	for {
		t.Logf("====== LOOP %d\n", i)

		// create consensus state from a clean slate
		logger := log.NewNopLogger()
		blockDB := dbm.NewMemDB()
		stateDB := dbm.NewMemDB()
		stateStore := sm.NewStore(stateDB)
		blockStore := store.NewBlockStore(blockDB)
		state, err := sm.MakeGenesisStateFromFile(consensusReplayConfig.GenesisFile())
		require.NoError(t, err)
		privValidator := loadPrivValidator(consensusReplayConfig)
		cs := newStateWithConfigAndBlockStore(
			consensusReplayConfig,
			state,
			privValidator,
			kvstore.NewApplication(),
			blockStore,
		)
		cs.SetLogger(logger)

		// start sending transactions
		ctx, cancel := context.WithCancel(context.Background())
		initFn(stateDB, cs, ctx)

		// clean up WAL file from the previous iteration
		walFile := cs.config.WalFile()
		os.Remove(walFile)

		// set crashing WAL
		csWal, err := cs.OpenWAL(walFile)
		require.NoError(t, err)
		crashingWal.next = csWal

		// reset the message counter
		crashingWal.msgIndex = 1
		cs.wal = crashingWal

		// start consensus state
		err = cs.Start()
		require.NoError(t, err)

		i++

		select {
		case err := <-walPanicked:
			t.Logf("WAL panicked: %v", err)

			// make sure we can make blocks after a crash
			startNewStateAndWaitForBlock(t, consensusReplayConfig, cs.Height, blockDB, stateStore)

			// stop consensus state and transactions sender (initFn)
			cs.Stop() //nolint:errcheck // Logging this error causes failure
			cancel()

			// if we reached the required height, exit
			if _, ok := err.(ReachedHeightToStopError); ok {
				break LOOP
			}
		case <-time.After(10 * time.Second):
			t.Fatal("WAL did not panic for 10 seconds (check the log)")
		}
	}
}

// crashingWAL is a WAL which crashes or rather simulates a crash during Save
// (before and after). It remembers a message for which we last panicked
// (lastPanickedForMsgIndex), so we don't panic for it in subsequent iterations.
type crashingWAL struct {
	next         WAL
	panicCh      chan error
	heightToStop int64

	msgIndex                int // current message index
	lastPanickedForMsgIndex int // last message for which we panicked
}

var _ WAL = &crashingWAL{}

// WALWriteError indicates a WAL crash.
type WALWriteError struct {
	msg string
}

func (e WALWriteError) Error() string {
	return e.msg
}

// ReachedHeightToStopError indicates we've reached the required consensus
// height and may exit.
type ReachedHeightToStopError struct {
	height int64
}

func (e ReachedHeightToStopError) Error() string {
	return fmt.Sprintf("reached height to stop %d", e.height)
}

// Write simulate WAL's crashing by sending an error to the panicCh and then
// exiting the cs.receiveRoutine.
func (w *crashingWAL) Write(m WALMessage) error {
	if endMsg, ok := m.(EndHeightMessage); ok {
		if endMsg.Height == w.heightToStop {
			w.panicCh <- ReachedHeightToStopError{endMsg.Height}
			runtime.Goexit()
			return nil
		}

		return w.next.Write(m)
	}

	if w.msgIndex > w.lastPanickedForMsgIndex {
		w.lastPanickedForMsgIndex = w.msgIndex
		_, file, line, _ := runtime.Caller(1)
		w.panicCh <- WALWriteError{fmt.Sprintf("failed to write %T to WAL (fileline: %s:%d)", m, file, line)}
		runtime.Goexit()
		return nil
	}

	w.msgIndex++
	return w.next.Write(m)
}

func (w *crashingWAL) WriteSync(m WALMessage) error {
	return w.Write(m)
}

func (w *crashingWAL) FlushAndSync() error { return w.next.FlushAndSync() }

func (w *crashingWAL) SearchForEndHeight(
	height int64,
	options *WALSearchOptions) (rd io.ReadCloser, found bool, err error) {
	return w.next.SearchForEndHeight(height, options)
}

func (w *crashingWAL) Start() error { return w.next.Start() }
func (w *crashingWAL) Stop() error  { return w.next.Stop() }
func (w *crashingWAL) Wait()        { w.next.Wait() }

//------------------------------------------------------------------------------------------
type simulatorTestSuite struct {
	GenesisState sm.State
	Config       *config.Config
	Chain        []*types.Block
	Commits      []*types.Commit
	CleanupFunc  cleanupFunc

	Mempool mempl.Mempool
	Evpool  sm.EvidencePool
}

const (
	numBlocks = 6
)

//---------------------------------------
// Test handshake/replay

// 0 - all synced up
// 1 - saved block but app and state are behind
// 2 - save block and committed but state is behind
// 3 - save block and committed with truncated block store and state behind
var modes = []uint{0, 1, 2, 3}

func findProposer(validatorStubs []*validatorStub, proTxHash crypto.ProTxHash) *validatorStub {
	for _, validatorStub := range validatorStubs {
		valProTxHash, _ := validatorStub.GetProTxHash(context.Background())
		if bytes.Equal(valProTxHash, proTxHash) {
			return validatorStub
		}
	}
	panic("validator not found")
}

// This is actually not a test, it's for storing validator change tx data for testHandshakeReplay
func setupSimulator(t *testing.T) *simulatorTestSuite {
	t.Helper()
	cfg := configSetup(t)

	sim := &simulatorTestSuite{
		Mempool: emptyMempool{},
		Evpool:  sm.EmptyEvidencePool{},
	}

	nPeers := 7
	nVals := 4
	css, genDoc, cfg, cleanup := randConsensusNetWithPeers(
		cfg,
		nVals,
		nPeers,
		"replay_test",
		newMockTickerFunc(true),
		newPersistentKVStoreWithPath)
	fmt.Printf("initial quorum hash is %X\n", genDoc.QuorumHash)
	sim.Config = cfg
	sim.GenesisState, _ = sm.MakeGenesisState(genDoc)
	sim.CleanupFunc = cleanup

	partSize := types.BlockPartSizeBytes

	newRoundCh := subscribe(css[0].eventBus, types.EventQueryNewRound)
	proposalCh := subscribe(css[0].eventBus, types.EventQueryCompleteProposal)

	vss := make([]*validatorStub, nPeers)
	for i := 0; i < nPeers; i++ {
		vss[i] = newValidatorStub(css[i].privValidator, int32(i), genDoc.InitialHeight)
	}
	height, round := css[0].Height, css[0].Round

	// start the machine; note height should be equal to InitialHeight here,
	// so we don't need to increment it
	startTestRound(css[0], height, round)
	ensureNewRound(newRoundCh, height, 0)
	ensureNewProposal(proposalCh, height, round)

	// Stop auto proposing blocks, as this could lead to issues based on the
	// randomness of proposer selection
	css[0].config.DontAutoPropose = true

	rs := css[0].GetRoundState()
	signAddVotes(
		sim.Config,
		css[0],
		tmproto.PrevoteType,
		rs.ProposalBlock.Hash(),
		rs.ProposalBlockParts.Header(),
		vss[1:nVals]...)
	signAddVotes(
		sim.Config,
		css[0],
		tmproto.PrecommitType,
		rs.ProposalBlock.Hash(),
		rs.ProposalBlockParts.Header(),
		vss[1:nVals]...)
	ensureNewRound(newRoundCh, height+1, 0)

	// HEIGHT 2
	updatedValidators2, _, newThresholdPublicKey, quorumHash2 := updateConsensusNetAddNewValidators(
		css,
		height,
		1,
		false,
	)
	height++
	fmt.Printf("quorum hash is now %X\n", quorumHash2)
	incrementHeight(vss...)
	updateTransactions := make([][]byte, len(updatedValidators2)+2)
	for i := 0; i < len(updatedValidators2); i++ {
		// start by adding all validator transactions
		abciPubKey, err := cryptoenc.PubKeyToProto(updatedValidators2[i].PubKey)
		require.NoError(t, err)
		updateTransactions[i] = kvstore.MakeValSetChangeTx(
			updatedValidators2[i].ProTxHash,
			&abciPubKey,
			testMinPower,
		)
	}
	abciThresholdPubKey, err := cryptoenc.PubKeyToProto(newThresholdPublicKey)
	require.NoError(t, err)
	updateTransactions[len(updatedValidators2)] = kvstore.MakeThresholdPublicKeyChangeTx(
		abciThresholdPubKey,
	)
	updateTransactions[len(updatedValidators2)+1] = kvstore.MakeQuorumHashTx(quorumHash2)
	for _, updateTransaction := range updateTransactions {
		err = assertMempool(css[0].txNotifier).CheckTx(context.Background(), updateTransaction, nil, mempl.TxInfo{})
		assert.Nil(t, err)
	}

	propBlock, _ := css[0].createProposalBlock() // changeProposer(t, cs1, vs2)
	propBlockParts := propBlock.MakePartSet(partSize)
	blockID := types.BlockID{Hash: propBlock.Hash(), PartSetHeader: propBlockParts.Header()}
	// stateID := types.StateID{LastAppHash: css[0].state.AppHash}

	proposal := types.NewProposal(vss[1].Height, 1, round, -1, blockID)
	p := proposal.ToProto()
	if _, err := vss[1].SignProposal(context.Background(), cfg.ChainID(), genDoc.QuorumType, genDoc.QuorumHash, p); err != nil {
		t.Fatal("failed to sign bad proposal", err)
	}
	proposal.Signature = p.Signature

	// set the proposal block to state on node 0, this will result in a signed prevote,
	// so we do not need to prevote with it again (hence the vss[1:nVals])
	if err := css[0].SetProposalAndBlock(proposal, propBlock, propBlockParts, "some peer"); err != nil {
		t.Fatal(err)
	}
	ensureNewProposal(proposalCh, height, round)
	rs = css[0].GetRoundState()
	signAddVotes(
		sim.Config,
		css[0],
		tmproto.PrevoteType,
		rs.ProposalBlock.Hash(),
		rs.ProposalBlockParts.Header(),
		vss[1:nVals]...)
	signAddVotes(
		sim.Config,
		css[0],
		tmproto.PrecommitType,
		rs.ProposalBlock.Hash(),
		rs.ProposalBlockParts.Header(),
		vss[1:nVals]...)
	ensureNewRound(newRoundCh, height+1, 0)

	// HEIGHT 3
	height++
	incrementHeight(vss...)
	propBlock, _ = css[0].createProposalBlock() // changeProposer(t, cs1, vs2)
	propBlockParts = propBlock.MakePartSet(partSize)
	blockID = types.BlockID{Hash: propBlock.Hash(), PartSetHeader: propBlockParts.Header()}

	proposal = types.NewProposal(vss[2].Height, 1, round, -1, blockID)
	p = proposal.ToProto()
	if _, err := vss[2].SignProposal(context.Background(), cfg.ChainID(), genDoc.QuorumType, genDoc.QuorumHash, p); err != nil {
		t.Fatal("failed to sign bad proposal", err)
	}
	proposal.Signature = p.Signature

	// set the proposal block
	if err := css[0].SetProposalAndBlock(proposal, propBlock, propBlockParts, "some peer"); err != nil {
		t.Fatal(err)
	}
	ensureNewProposal(proposalCh, height, round)
	rs = css[0].GetRoundState()
	signAddVotes(
		sim.Config,
		css[0],
		tmproto.PrevoteType,
		rs.ProposalBlock.Hash(),
		rs.ProposalBlockParts.Header(),
		vss[1:nVals]...)
	signAddVotes(
		sim.Config,
		css[0],
		tmproto.PrecommitType,
		rs.ProposalBlock.Hash(),
		rs.ProposalBlockParts.Header(),
		vss[1:nVals]...)
	ensureNewRound(newRoundCh, height+1, 0)

	// HEIGHT 4
	// 1 new validator comes in here from block 2
	updatedValidators4, _, newThresholdPublicKey, quorumHash4 := updateConsensusNetAddNewValidators(
		css,
		height,
		2,
		false,
	)
	height++
	incrementHeight(vss...)
	updateTransactions2 := make([][]byte, len(updatedValidators4)+2)
	for i := 0; i < len(updatedValidators4); i++ {
		// start by adding all validator transactions
		abciPubKey, err := cryptoenc.PubKeyToProto(updatedValidators4[i].PubKey)
		require.NoError(t, err)
		updateTransactions2[i] = kvstore.MakeValSetChangeTx(
			updatedValidators4[i].ProTxHash,
			&abciPubKey,
			testMinPower,
		)
		var oldPubKey crypto.PubKey
		for _, validatorAt2 := range updatedValidators2 {
			if bytes.Equal(validatorAt2.ProTxHash, updatedValidators4[i].ProTxHash) {
				oldPubKey = validatorAt2.PubKey
			}
		}
		fmt.Printf(
			"update at height 4 (for 6) %v %v -> %v\n",
			updatedValidators4[i].ProTxHash,
			oldPubKey,
			updatedValidators4[i].PubKey,
		)
	}
	abciThresholdPubKey2, err := cryptoenc.PubKeyToProto(newThresholdPublicKey)
	require.NoError(t, err)
	updateTransactions2[len(updatedValidators4)] = kvstore.MakeThresholdPublicKeyChangeTx(
		abciThresholdPubKey2,
	)
	updateTransactions2[len(updatedValidators4)+1] = kvstore.MakeQuorumHashTx(quorumHash4)
	for _, updateTransaction := range updateTransactions2 {
		err = assertMempool(css[0].txNotifier).CheckTx(context.Background(), updateTransaction, nil, mempl.TxInfo{})
		assert.Nil(t, err)
	}
	propBlock, _ = css[0].createProposalBlock() // changeProposer(t, cs1, vs2)
	propBlockParts = propBlock.MakePartSet(partSize)
	if len(propBlock.Txs) != 9 {
		panic("there should be 9 transactions")
	}
	blockID = types.BlockID{Hash: propBlock.Hash(), PartSetHeader: propBlockParts.Header()}

	vssProposer := findProposer(vss, css[0].Validators.Proposer.ProTxHash)
	proposal = types.NewProposal(vss[3].Height, 1, round, -1, blockID)
	p = proposal.ToProto()
	if _, err := vssProposer.SignProposal(context.Background(), cfg.ChainID(), genDoc.QuorumType, quorumHash2, p); err != nil {
		t.Fatal("failed to sign bad proposal", err)
	}
	proposal.Signature = p.Signature

	// set the proposal block
	if err := css[0].SetProposalAndBlock(proposal, propBlock, propBlockParts, "some peer"); err != nil {
		t.Fatal(err)
	}
	ensureNewProposal(proposalCh, height, round)
	rs = css[0].GetRoundState()
	vssForSigning := vss[0 : nVals+1]
	sort.Sort(ValidatorStubsByPower(vssForSigning))

	valIndexFn := func(cssIdx int) int {
		for i, vs := range vssForSigning {
			vsProTxHash, err := vs.GetProTxHash(context.Background())
			require.NoError(t, err)

			cssProTxHash, err := css[cssIdx].privValidator.GetProTxHash(context.Background())
			require.NoError(t, err)

			if bytes.Equal(vsProTxHash, cssProTxHash) {
				return i
			}
		}
		panic(fmt.Sprintf("validator css[%d] not found in newVss", cssIdx))
	}

	selfIndex := valIndexFn(0)

	// A new validator should come in
	for i := 0; i < nVals+1; i++ {
		if i == selfIndex {
			continue
		}
		signAddVotes(
			sim.Config,
			css[0],
			tmproto.PrevoteType,
			rs.ProposalBlock.Hash(),
			rs.ProposalBlockParts.Header(),
			vssForSigning[i],
		)
	}
	for i := 0; i < nVals+1; i++ {
		if i == selfIndex {
			continue
		}
		signAddVotes(
			sim.Config,
			css[0],
			tmproto.PrecommitType,
			rs.ProposalBlock.Hash(),
			rs.ProposalBlockParts.Header(),
			vssForSigning[i],
		)
	}
	ensureNewRound(newRoundCh, height+1, 0)

	// HEIGHT 5
	height++
	incrementHeight(vss...)
	propBlock, _ = css[0].createProposalBlock() // changeProposer(t, cs1, vs2)
	propBlockParts = propBlock.MakePartSet(partSize)
	blockID = types.BlockID{Hash: propBlock.Hash(), PartSetHeader: propBlockParts.Header()}

	proposal = types.NewProposal(vss[2].Height, 1, round, -1, blockID)
	p = proposal.ToProto()
	proposerProTxHash := css[0].RoundState.Validators.GetProposer().ProTxHash
	valIndexFnByProTxHash := func(proTxHash crypto.ProTxHash) int {
		for i, vs := range vss {
			vsProTxHash, err := vs.GetProTxHash(context.Background())
			require.NoError(t, err)

			if bytes.Equal(vsProTxHash, proposerProTxHash) {
				return i
			}
		}
		panic(fmt.Sprintf("validator proTxHash %X not found in newVss", proposerProTxHash))
	}
	proposerIndex := valIndexFnByProTxHash(proposerProTxHash)
	if _, err := vss[proposerIndex].SignProposal(context.Background(), cfg.ChainID(),
		genDoc.QuorumType, quorumHash2, p); err != nil {
		t.Fatal("failed to sign bad proposal", err)
	}
	proposal.Signature = p.Signature

	// set the proposal block
	if err := css[0].SetProposalAndBlock(proposal, propBlock, propBlockParts, "some peer"); err != nil {
		t.Fatal(err)
	}
	ensureNewProposal(proposalCh, height, round)
	rs = css[0].GetRoundState()
	for i := 0; i < nVals+1; i++ {
		if i == selfIndex {
			continue
		}
		signAddVotes(
			sim.Config,
			css[0],
			tmproto.PrevoteType,
			rs.ProposalBlock.Hash(),
			rs.ProposalBlockParts.Header(),
			vssForSigning[i],
		)
	}
	for i := 0; i < nVals+1; i++ {
		if i == selfIndex {
			continue
		}
		signAddVotes(
			sim.Config,
			css[0],
			tmproto.PrecommitType,
			rs.ProposalBlock.Hash(),
			rs.ProposalBlockParts.Header(),
			vssForSigning[i],
		)
	}
	ensureNewRound(newRoundCh, height+1, 0)

	// HEIGHT 6
	//
	updatedValidators6, _, newThresholdPublicKey, quorumHash6 := updateConsensusNetRemoveValidators(
		css,
		height,
		1,
		false,
	)
	height++
	updateTransactions3 := make([][]byte, len(updatedValidators6)+2)
	for i := 0; i < len(updatedValidators6); i++ {
		// start by adding all validator transactions
		abciPubKey, err := cryptoenc.PubKeyToProto(updatedValidators6[i].PubKey)
		require.NoError(t, err)
		updateTransactions3[i] = kvstore.MakeValSetChangeTx(
			updatedValidators6[i].ProTxHash,
			&abciPubKey,
			testMinPower,
		)
		var oldPubKey crypto.PubKey
		for _, validatorAt4 := range updatedValidators4 {
			if bytes.Equal(validatorAt4.ProTxHash, updatedValidators6[i].ProTxHash) {
				oldPubKey = validatorAt4.PubKey
			}
		}
		fmt.Printf(
			"update at height 6 (for 8) %v %v -> %v\n",
			updatedValidators6[i].ProTxHash,
			oldPubKey,
			updatedValidators6[i].PubKey,
		)
	}
	abciThresholdPubKey, err = cryptoenc.PubKeyToProto(newThresholdPublicKey)
	require.NoError(t, err)
	updateTransactions3[len(updatedValidators6)] =
		kvstore.MakeThresholdPublicKeyChangeTx(abciThresholdPubKey)
	updateTransactions3[len(updatedValidators6)+1] = kvstore.MakeQuorumHashTx(quorumHash6)

	for _, updateTransaction := range updateTransactions3 {
		err = assertMempool(css[0].txNotifier).CheckTx(context.Background(), updateTransaction, nil, mempl.TxInfo{})
		assert.Nil(t, err)
	}
	incrementHeight(vss...)
	propBlock, _ = css[0].createProposalBlock() // changeProposer(t, cs1, vs2)
	propBlockParts = propBlock.MakePartSet(partSize)
	blockID = types.BlockID{Hash: propBlock.Hash(), PartSetHeader: propBlockParts.Header()}

	proposal = types.NewProposal(vss[2].Height, 1, round, -1, blockID)
	p = proposal.ToProto()
	proposer := css[0].RoundState.Validators.GetProposer()
	proposerProTxHash = proposer.ProTxHash
	proposerPubKey := proposer.PubKey
	valIndexFnByProTxHash = func(proTxHash crypto.ProTxHash) int {
		for i, vs := range vss {
			vsProTxHash, err := vs.GetProTxHash(context.Background())
			require.NoError(t, err)

			if bytes.Equal(vsProTxHash, proposerProTxHash) {
				return i
			}
		}
		panic(fmt.Sprintf(
			"validator proTxHash %X not found in newVss",
			proposerProTxHash,
		))
	}
	proposerIndex = valIndexFnByProTxHash(proposerProTxHash)
	validatorsAtProposalHeight := css[0].state.ValidatorsAtHeight(p.Height)

	signID, err :=
		vss[proposerIndex].SignProposal(
			context.Background(),
			cfg.ChainID(),
			genDoc.QuorumType,
			validatorsAtProposalHeight.QuorumHash,
			p,
		)
	if err != nil {
		t.Fatal("failed to sign bad proposal", err)
	}

	proposerPubKey2, err := vss[proposerIndex].GetPubKey(context.Background(), validatorsAtProposalHeight.QuorumHash)
	if err != nil {
		t.Fatal("failed to get public key")
	}
	proposerProTxHash2, err := vss[proposerIndex].GetProTxHash(context.Background())

	if !bytes.Equal(proposerProTxHash2.Bytes(), proposerProTxHash.Bytes()) {
		t.Fatal("wrong proposer", err)
	}

	if !bytes.Equal(proposerPubKey2.Bytes(), proposerPubKey.Bytes()) {
		t.Fatal("wrong proposer pubKey", err)
	}

	css[0].Logger.Debug(
		"signed proposal", "height", proposal.Height, "round", proposal.Round,
		"proposer", proposerProTxHash.ShortString(), "signature", p.Signature,
		"pubkey", proposerPubKey2.Bytes(), "quorum type",
		validatorsAtProposalHeight.QuorumType, "quorum hash",
		validatorsAtProposalHeight.QuorumHash, "signID", signID,
	)

	proposal.Signature = p.Signature

	// set the proposal block
	if err := css[0].SetProposalAndBlock(proposal, propBlock, propBlockParts, "some peer"); err != nil {
		t.Fatal(err)
	}
	ensureNewProposal(proposalCh, height, round)

	vssForSigning = vss[0 : nVals+3]
	sort.Sort(ValidatorStubsByPower(vssForSigning))

	selfIndex = valIndexFn(0)

	// All validators should be in now
	rs = css[0].GetRoundState()
	for i := 0; i < nVals+3; i++ {
		if i == selfIndex {
			continue
		}
		signAddVotes(
			sim.Config,
			css[0],
			tmproto.PrevoteType,
			rs.ProposalBlock.Hash(),
			rs.ProposalBlockParts.Header(),
			vssForSigning[i],
		)
	}
	for i := 0; i < nVals+3; i++ {
		if i == selfIndex {
			continue
		}
		signAddVotes(
			sim.Config,
			css[0],
			tmproto.PrecommitType,
			rs.ProposalBlock.Hash(),
			rs.ProposalBlockParts.Header(),
			vssForSigning[i],
		)
	}

	ensureNewRound(newRoundCh, height+1, 0)

	// HEIGHT 7
	height++
	incrementHeight(vss...)
	propBlock, _ = css[0].createProposalBlock() // changeProposer(t, cs1, vs2)
	propBlockParts = propBlock.MakePartSet(partSize)
	blockID = types.BlockID{Hash: propBlock.Hash(), PartSetHeader: propBlockParts.Header()}

	proposal = types.NewProposal(vss[2].Height, 1, round, -1, blockID)
	p = proposal.ToProto()
	proposerProTxHash = css[0].RoundState.Validators.GetProposer().ProTxHash
	proposerIndex = valIndexFnByProTxHash(proposerProTxHash)
	validatorsAtProposalHeight = css[0].state.ValidatorsAtHeight(p.Height)
	if _, err := vss[proposerIndex].SignProposal(
		context.Background(),
		cfg.ChainID(),
		genDoc.QuorumType,
		validatorsAtProposalHeight.QuorumHash,
		p,
	); err != nil {
		t.Fatal("failed to sign bad proposal", err)
	}
	proposal.Signature = p.Signature

	// set the proposal block
	if err := css[0].SetProposalAndBlock(proposal, propBlock, propBlockParts, "some peer"); err != nil {
		t.Fatal(err)
	}
	selfIndex = valIndexFn(0)
	ensureNewProposal(proposalCh, height, round)
	rs = css[0].GetRoundState()

	// Still have 7 validators
	for i := 0; i < nVals+3; i++ {
		if i == selfIndex {
			continue
		}
		signAddVotes(
			sim.Config,
			css[0],
			tmproto.PrevoteType,
			rs.ProposalBlock.Hash(),
			rs.ProposalBlockParts.Header(),
			vssForSigning[i],
		)
	}
	for i := 0; i < nVals+3; i++ {
		if i == selfIndex {
			continue
		}
		signAddVotes(
			sim.Config,
			css[0],
			tmproto.PrecommitType,
			rs.ProposalBlock.Hash(),
			rs.ProposalBlockParts.Header(),
			vssForSigning[i],
		)
	}
	ensureNewRound(newRoundCh, height+1, 0)

	// HEIGHT 8

	proTxHashToRemove := updatedValidators6[len(updatedValidators6)-1].ProTxHash
	updatedValidators8, _, newThresholdPublicKey, quorumHash8 := updateConsensusNetRemoveValidatorsWithProTxHashes(
		css,
		height,
		[]crypto.ProTxHash{proTxHashToRemove},
		false,
	)
	height++
	incrementHeight(vss...)

	updateTransactions4 := make([][]byte, len(updatedValidators8)+2)
	for i := 0; i < len(updatedValidators8); i++ {
		// start by adding all validator transactions
		abciPubKey, err := cryptoenc.PubKeyToProto(updatedValidators8[i].PubKey)
		require.NoError(t, err)
		updateTransactions4[i] = kvstore.MakeValSetChangeTx(
			updatedValidators8[i].ProTxHash, &abciPubKey, testMinPower,
		)
	}
	abciThresholdPubKey, err = cryptoenc.PubKeyToProto(newThresholdPublicKey)
	require.NoError(t, err)
	updateTransactions4[len(updatedValidators8)] = kvstore.MakeThresholdPublicKeyChangeTx(
		abciThresholdPubKey,
	)
	updateTransactions4[len(updatedValidators8)+1] = kvstore.MakeQuorumHashTx(quorumHash8)

	for _, updateTransaction := range updateTransactions4 {
		err = assertMempool(css[0].txNotifier).CheckTx(context.Background(),
			updateTransaction, nil, mempl.TxInfo{})
		assert.Nil(t, err)
	}
	propBlock, _ = css[0].createProposalBlock() // changeProposer(t, cs1, vs2)
	propBlockParts = propBlock.MakePartSet(partSize)
	blockID = types.BlockID{Hash: propBlock.Hash(), PartSetHeader: propBlockParts.Header()}

	proposal = types.NewProposal(vss[5].Height, 1, round, -1, blockID)
	p = proposal.ToProto()
	proposer = css[0].RoundState.Validators.GetProposer()
	proposerProTxHash = proposer.ProTxHash
	proposerPubKey = proposer.PubKey
	proposerIndex = valIndexFnByProTxHash(proposerProTxHash)
	validatorsAtProposalHeight = css[0].state.ValidatorsAtHeight(p.Height)
	signID, err = vss[proposerIndex].SignProposal(
		context.Background(),
		cfg.ChainID(),
		genDoc.QuorumType,
		validatorsAtProposalHeight.QuorumHash,
		p,
	)

	if err != nil {
		t.Fatal("failed to sign bad proposal", err)
	}

	// proposerPubKey2, _ = vss[proposerIndex].GetPubKey(validatorsAtProposalHeight.QuorumHash)

	/*
		if !bytes.Equal(proposerPubKey2.Bytes(), proposerPubKey.Bytes()) {
			//t.Fatal("wrong proposer pubKey", err)
		}*/

	css[0].Logger.Debug(
		"signed proposal", "height", proposal.Height, "round", proposal.Round,
		"proposer", proposerProTxHash.ShortString(), "signature", p.Signature,
		"pubkey", proposerPubKey.Bytes(), "quorum type",
		validatorsAtProposalHeight.QuorumType, "quorum hash",
		validatorsAtProposalHeight.QuorumHash, "signID", signID)

	proposal.Signature = p.Signature

	// set the proposal block
	if err := css[0].SetProposalAndBlock(proposal, propBlock, propBlockParts, "some peer"); err != nil {
		t.Fatal(err)
	}

	ensureNewProposal(proposalCh, height, round)
	rs = css[0].GetRoundState()
	// Reflect the changes to vss[nVals] at height 3 and resort newVss.
	vssForSigning = vss[0 : nVals+3]
	sort.Sort(ValidatorStubsByPower(vssForSigning))
	vssForSigning = vssForSigning[0 : nVals+2]
	selfIndex = valIndexFn(0)
	for i := 0; i < nVals+2; i++ {
		if i == selfIndex {
			continue
		}
		signAddVotes(
			sim.Config,
			css[0],
			tmproto.PrevoteType,
			rs.ProposalBlock.Hash(),
			rs.ProposalBlockParts.Header(),
			vssForSigning[i],
		)
	}
	for i := 0; i < nVals+2; i++ {
		if i == selfIndex {
			continue
		}
		signAddVotes(
			sim.Config,
			css[0],
			tmproto.PrecommitType,
			rs.ProposalBlock.Hash(),
			rs.ProposalBlockParts.Header(),
			vssForSigning[i],
		)
	}
	ensureNewRound(newRoundCh, height+1, 0)

	sim.Chain = make([]*types.Block, 0)
	sim.Commits = make([]*types.Commit, 0)
	for i := 1; i <= numBlocks; i++ {
		sim.Chain = append(sim.Chain, css[0].blockStore.LoadBlock(int64(i)))
		sim.Commits = append(sim.Commits, css[0].blockStore.LoadBlockCommit(int64(i)))
	}
	if sim.CleanupFunc != nil {
		t.Cleanup(sim.CleanupFunc)
	}

	return sim
}

// Sync from scratch
func TestHandshakeReplayAll(t *testing.T) {
	sim := setupSimulator(t)

	for _, m := range modes {
		testHandshakeReplay(t, sim, 0, m, false)
	}
	for _, m := range modes {
		testHandshakeReplay(t, sim, 0, m, true)
	}
}

// Sync many, not from scratch
func TestHandshakeReplaySome(t *testing.T) {
	sim := setupSimulator(t)

	for _, m := range modes {
		testHandshakeReplay(t, sim, 2, m, false)
	}
	for _, m := range modes {
		testHandshakeReplay(t, sim, 2, m, true)
	}
}

// Sync from lagging by one
func TestHandshakeReplayOne(t *testing.T) {
	sim := setupSimulator(t)

	for _, m := range modes {
		testHandshakeReplay(t, sim, numBlocks-1, m, false)
	}
	for _, m := range modes {
		testHandshakeReplay(t, sim, numBlocks-1, m, true)
	}
}

// Sync from caught up
func TestHandshakeReplayNone(t *testing.T) {
	sim := setupSimulator(t)

	for _, m := range modes {
		testHandshakeReplay(t, sim, numBlocks, m, false)
	}
	for _, m := range modes {
		testHandshakeReplay(t, sim, numBlocks, m, true)
	}
}

// Test mockProxyApp should not panic when app return ABCIResponses with some empty ResponseDeliverTx
func TestMockProxyApp(t *testing.T) {
	sim := setupSimulator(t) // setup config and simulator
	cfg := sim.Config
	assert.NotNil(t, cfg)

	logger := log.TestingLogger()
	var validTxs, invalidTxs = 0, 0
	txIndex := 0

	assert.NotPanics(t, func() {
		abciResWithEmptyDeliverTx := new(tmstate.ABCIResponses)
		abciResWithEmptyDeliverTx.DeliverTxs = make([]*abci.ResponseDeliverTx, 0)
		abciResWithEmptyDeliverTx.DeliverTxs = append(
			abciResWithEmptyDeliverTx.DeliverTxs,
			&abci.ResponseDeliverTx{},
		)

		// called when saveABCIResponses:
		bytes, err := proto.Marshal(abciResWithEmptyDeliverTx)
		require.NoError(t, err)
		loadedAbciRes := new(tmstate.ABCIResponses)

		// this also happens sm.LoadABCIResponses
		err = proto.Unmarshal(bytes, loadedAbciRes)
		require.NoError(t, err)

		mock := newMockProxyApp([]byte("mock_hash"), loadedAbciRes)

		abciRes := new(tmstate.ABCIResponses)
		abciRes.DeliverTxs = make([]*abci.ResponseDeliverTx, len(loadedAbciRes.DeliverTxs))
		// Execute transactions and get hash.
		proxyCb := func(req *abci.Request, res *abci.Response) {
			if r, ok := res.Value.(*abci.Response_DeliverTx); ok {
				// TODO: make use of res.Log
				// TODO: make use of this info
				// Blocks may include invalid txs.
				txRes := r.DeliverTx
				if txRes.Code == abci.CodeTypeOK {
					validTxs++
				} else {
					logger.Debug("Invalid tx", "code", txRes.Code, "log", txRes.Log)
					invalidTxs++
				}
				abciRes.DeliverTxs[txIndex] = txRes
				txIndex++
			}
		}
		mock.SetResponseCallback(proxyCb)

		someTx := []byte("tx")
		_, err = mock.DeliverTxAsync(context.Background(), abci.RequestDeliverTx{Tx: someTx})
		assert.NoError(t, err)
	})
	assert.True(t, validTxs == 1)
	assert.True(t, invalidTxs == 0)
}

func tempWALWithData(data []byte) string {
	walFile, err := ioutil.TempFile("", "wal")
	if err != nil {
		panic(fmt.Sprintf("failed to create temp WAL file: %v", err))
	}
	_, err = walFile.Write(data)
	if err != nil {
		panic(fmt.Sprintf("failed to write to temp WAL file: %v", err))
	}
	if err := walFile.Close(); err != nil {
		panic(fmt.Sprintf("failed to close temp WAL file: %v", err))
	}
	return walFile.Name()
}

// Make some blocks. Start a fresh app and apply nBlocks blocks.
// Then restart the app and sync it up with the remaining blocks
func testHandshakeReplay(
	t *testing.T,
	sim *simulatorTestSuite,
	nBlocks int,
	mode uint,
	testValidatorsChange bool,
) {
	var chain []*types.Block
	var commits []*types.Commit
	var store *mockBlockStore
	var stateDB dbm.DB
	var genesisState sm.State
	var privVal types.PrivValidator

	cfg := sim.Config

	if testValidatorsChange {
		testConfig, err := ResetConfig(fmt.Sprintf("%s_%v_m", t.Name(), mode))
		require.NoError(t, err)
		defer func() { _ = os.RemoveAll(testConfig.RootDir) }()
		stateDB = dbm.NewMemDB()

		genesisState = sim.GenesisState
		cfg = sim.Config
		chain = append([]*types.Block{}, sim.Chain...) // copy chain
		commits = sim.Commits
		store = newMockBlockStore(cfg, genesisState.ConsensusParams)
		privVal, err = privval.LoadFilePV(cfg.PrivValidator.KeyFile(), cfg.PrivValidator.StateFile())
		require.NoError(t, err)
	} else { // test single node
		testConfig, err := ResetConfig(fmt.Sprintf("%s_%v_s", t.Name(), mode))
		require.NoError(t, err)
		defer func() { _ = os.RemoveAll(testConfig.RootDir) }()
		walBody, err := WALWithNBlocks(t, numBlocks)
		require.NoError(t, err)
		walFile := tempWALWithData(walBody)
		cfg.Consensus.SetWalFile(walFile)

		privVal, err = privval.LoadFilePV(cfg.PrivValidator.KeyFile(), cfg.PrivValidator.StateFile())
		require.NoError(t, err)

		gdoc, err := sm.MakeGenesisDocFromFile(cfg.GenesisFile())
		if err != nil {
			t.Error(err)
		}

		wal, err := NewWAL(walFile)
		require.NoError(t, err)
		wal.SetLogger(log.TestingLogger())
		err = wal.Start()
		require.NoError(t, err)
		t.Cleanup(func() {
			if err := wal.Stop(); err != nil {
				t.Error(err)
			}
		})
		chain, commits, err = makeBlockchainFromWAL(wal, gdoc)
		require.NoError(t, err)
		pubKey, err := privVal.GetPubKey(context.Background(), gdoc.QuorumHash)
		require.NoError(t, err)
		stateDB, genesisState, store = stateAndStore(cfg, pubKey, kvstore.ProtocolVersion)

	}
	proTxHash, err := privVal.GetProTxHash(context.Background())
	require.NoError(t, err)

	stateStore := sm.NewStore(stateDB)
	store.chain = chain
	store.commits = commits

	state := genesisState.Copy()
	firstValidatorProTxHash, _ := state.Validators.GetByIndex(0)
	// run the chain through state.ApplyBlock to build up the tendermint state
	state = buildTMStateFromChain(
		cfg,
		sim.Mempool,
		sim.Evpool,
		stateStore,
		firstValidatorProTxHash,
		state,
		chain,
		nBlocks,
		mode,
		store,
	)
	latestAppHash := state.AppHash

	// make a new client creator
	kvstoreApp := kvstore.NewPersistentKVStoreApplication(
		filepath.Join(cfg.DBDir(), fmt.Sprintf("replay_test_%d_%d_a_r%d", nBlocks, mode, rand.Int())))
	t.Cleanup(func() { require.NoError(t, kvstoreApp.Close()) })

	clientCreator2 := abciclient.NewLocalCreator(kvstoreApp)
	if nBlocks > 0 {
		// run nBlocks against a new client to build up the app state.
		// use a throwaway tendermint state
		proxyApp := proxy.NewAppConns(clientCreator2)
		stateDB1 := dbm.NewMemDB()
		stateStore := sm.NewStore(stateDB1)
		err := stateStore.Save(genesisState)
		require.NoError(t, err)
		buildAppStateFromChain(
			proxyApp,
			stateStore,
			firstValidatorProTxHash,
			sim.Mempool,
			sim.Evpool,
			genesisState,
			chain,
			nBlocks,
			mode,
			store,
		)
	}

	// Prune block store if requested
	expectError := false
	if mode == 3 {
		pruned, err := store.PruneBlocks(2)
		require.NoError(t, err)
		require.EqualValues(t, 1, pruned)
		expectError = int64(nBlocks) < 2
	}

	// now start the app using the handshake - it should sync
	genDoc, _ := sm.MakeGenesisDocFromFile(cfg.GenesisFile())
	handshaker := NewHandshaker(
		stateStore,
		state,
		store,
		genDoc,
		proTxHash,
		cfg.Consensus.AppHashSize,
	)
	proxyApp := proxy.NewAppConns(clientCreator2)
	if err := proxyApp.Start(); err != nil {
		t.Fatalf("Error starting proxy app connections: %v", err)
	}

	t.Cleanup(func() {
		if err := proxyApp.Stop(); err != nil {
			t.Error(err)
		}
	})

	_, err = handshaker.Handshake(proxyApp)
	if expectError {
		require.Error(t, err)
		return
	} else if err != nil {
		t.Fatalf("Error on abci handshake: %v", err)
	}

	// get the latest app hash from the app
	res, err := proxyApp.Query().InfoSync(context.Background(), abci.RequestInfo{Version: ""})
	if err != nil {
		t.Fatal(err)
	}

	// the app hash should be synced up
	if !bytes.Equal(latestAppHash, res.LastBlockAppHash) {
		t.Fatalf(
			"Expected app hashes to match after handshake/replay. got %X, expected %X",
			res.LastBlockAppHash,
			latestAppHash)
	}

	expectedBlocksToSync := numBlocks - nBlocks
	if nBlocks == numBlocks && mode > 0 {
		expectedBlocksToSync++
	} else if nBlocks > 0 && mode == 1 {
		expectedBlocksToSync++
	}

	if handshaker.NBlocks() != expectedBlocksToSync {
		t.Fatalf(
			"Expected handshake to sync %d blocks, got %d",
			expectedBlocksToSync,
			handshaker.NBlocks(),
		)
	}
}

func applyBlock(
	stateStore sm.Store,
	mempool mempl.Mempool,
	evpool sm.EvidencePool,
	st sm.State,
	nodeProTxHash crypto.ProTxHash,
	blk *types.Block,
	proxyApp proxy.AppConns,
	blockStore *mockBlockStore,
) sm.State {
	testPartSize := types.BlockPartSizeBytes
	blockExec := sm.NewBlockExecutor(
		stateStore,
		log.TestingLogger(),
		proxyApp.Consensus(),
		proxyApp.Query(),
		mempool,
		evpool,
		blockStore,
		nil,
	)

	blkID := types.BlockID{Hash: blk.Hash(), PartSetHeader: blk.MakePartSet(testPartSize).Header()}
	newState, err := blockExec.ApplyBlock(st, nodeProTxHash, blkID, blk)
	if err != nil {
		panic(err)
	}
	return newState
}

func buildAppStateFromChain(
	proxyApp proxy.AppConns,
	stateStore sm.Store,
	nodeProTxHash crypto.ProTxHash,
	mempool mempl.Mempool,
	evpool sm.EvidencePool,
	state sm.State,
	chain []*types.Block,
	nBlocks int,
	mode uint,
	blockStore *mockBlockStore) {
	// start a new app without handshake, play nBlocks blocks
	if err := proxyApp.Start(); err != nil {
		panic(err)
	}
	defer proxyApp.Stop() //nolint:errcheck // ignore

	state.Version.Consensus.App = kvstore.ProtocolVersion // simulate handshake, receive app version
	validators := types.TM2PB.ValidatorUpdates(state.Validators)
	if _, err := proxyApp.Consensus().InitChainSync(context.Background(), abci.RequestInitChain{
		ValidatorSet: &validators,
	}); err != nil {
		panic(err)
	}
	if err := stateStore.Save(state); err != nil { // save height 1's validatorsInfo
		panic(err)
	}
	switch mode {
	case 0:
		for i := 0; i < nBlocks; i++ {
			block := chain[i]
			state = applyBlock(stateStore, mempool, evpool, state, nodeProTxHash, block, proxyApp, blockStore)
		}
	case 1, 2, 3:
		for i := 0; i < nBlocks-1; i++ {
			block := chain[i]
			state = applyBlock(stateStore, mempool, evpool, state, nodeProTxHash, block, proxyApp, blockStore)
		}

		if mode == 2 || mode == 3 {
			// update the kvstore height and apphash
			// as if we ran commit but not
			state = applyBlock(stateStore, mempool, evpool, state, nodeProTxHash, chain[nBlocks-1], proxyApp, blockStore)
		}
	default:
		panic(fmt.Sprintf("unknown mode %v", mode))
	}

}

func buildTMStateFromChain(
	cfg *config.Config,
	mempool mempl.Mempool,
	evpool sm.EvidencePool,
	stateStore sm.Store,
	nodeProTxHash crypto.ProTxHash,
	state sm.State,
	chain []*types.Block,
	nBlocks int,
	mode uint,
	blockStore *mockBlockStore) sm.State {
	// run the whole chain against this client to build up the tendermint state
	kvstoreApp := kvstore.NewPersistentKVStoreApplication(
		filepath.Join(cfg.DBDir(), fmt.Sprintf("replay_test_%d_%d_t", nBlocks, mode)))
	defer kvstoreApp.Close()
	clientCreator := abciclient.NewLocalCreator(kvstoreApp)

	proxyApp := proxy.NewAppConns(clientCreator)
	if err := proxyApp.Start(); err != nil {
		panic(err)
	}
	defer proxyApp.Stop() //nolint:errcheck

	state.Version.Consensus.App = kvstore.ProtocolVersion // simulate handshake, receive app version
	validators := types.TM2PB.ValidatorUpdates(state.Validators)
	if _, err := proxyApp.Consensus().InitChainSync(context.Background(), abci.RequestInitChain{
		ValidatorSet: &validators,
	}); err != nil {
		panic(err)
	}
	if err := stateStore.Save(state); err != nil { // save height 1's validatorsInfo
		panic(err)
	}
	switch mode {
	case 0:
		// sync right up
		for _, block := range chain {
			state = applyBlock(stateStore, mempool, evpool, state, nodeProTxHash, block, proxyApp, blockStore)
		}

	case 1, 2, 3:
		// sync up to the penultimate as if we stored the block.
		// whether we commit or not depends on the appHash
		for _, block := range chain[:len(chain)-1] {
			state = applyBlock(stateStore, mempool, evpool, state, nodeProTxHash, block, proxyApp, blockStore)
		}

		// apply the final block to a state copy so we can
		// get the right next appHash but keep the state back
		applyBlock(stateStore, mempool, evpool, state, nodeProTxHash, chain[len(chain)-1], proxyApp, blockStore)
	default:
		panic(fmt.Sprintf("unknown mode %v", mode))
	}

	return state
}

func TestHandshakePanicsIfAppReturnsWrongAppHash(t *testing.T) {
	// 1. Initialize tendermint and commit 3 blocks with the following app hashes:
	//		- 0x01
	//		- 0x02
	//		- 0x03
	cfg, err := ResetConfig("handshake_test_")
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(cfg.RootDir) })
	privVal, err := privval.LoadFilePV(cfg.PrivValidator.KeyFile(), cfg.PrivValidator.StateFile())
	require.NoError(t, err)
	const appVersion = 0x0
	quorumHash, err := privVal.GetFirstQuorumHash(context.Background())
	require.NoError(t, err)
	pubKey, err := privVal.GetPubKey(context.Background(), quorumHash)
	require.NoError(t, err)
	proTxHash, err := privVal.GetProTxHash(context.Background())
	require.NoError(t, err)
	stateDB, state, store := stateAndStore(cfg, pubKey, appVersion)
	stateStore := sm.NewStore(stateDB)
	genDoc, _ := sm.MakeGenesisDocFromFile(cfg.GenesisFile())
	state.LastValidators = state.Validators.Copy()
	// mode = 0 for committing all the blocks
	blocks, err := sf.MakeBlocks(3, &state, privVal)
	require.NoError(t, err)
	store.chain = blocks

	// 2. Tendermint must panic if app returns wrong hash for the first block
	//		- RANDOM HASH
	//		- 0x02
	//		- 0x03
	{
		app := &badApp{numBlocks: 3, allHashesAreWrong: true}
		clientCreator := abciclient.NewLocalCreator(app)
		proxyApp := proxy.NewAppConns(clientCreator)
		err := proxyApp.Start()
		require.NoError(t, err)
		t.Cleanup(func() {
			if err := proxyApp.Stop(); err != nil {
				t.Error(err)
			}
		})

		assert.Panics(t, func() {
			h := NewHandshaker(
				stateStore,
				state,
				store,
				genDoc,
				proTxHash,
				cfg.Consensus.AppHashSize,
			)
			if _, err = h.Handshake(proxyApp); err != nil {
				t.Log(err)
			}
		})
	}

	// 3. Tendermint must panic if app returns wrong hash for the last block
	//		- 0x01
	//		- 0x02
	//		- RANDOM HASH
	{
		app := &badApp{numBlocks: 3, onlyLastHashIsWrong: true}
		clientCreator := abciclient.NewLocalCreator(app)
		proxyApp := proxy.NewAppConns(clientCreator)
		err := proxyApp.Start()
		require.NoError(t, err)
		t.Cleanup(func() {
			if err := proxyApp.Stop(); err != nil {
				t.Error(err)
			}
		})

		assert.Panics(t, func() {
			h := NewHandshaker(
				stateStore,
				state,
				store,
				genDoc,
				proTxHash,
				cfg.Consensus.AppHashSize,
			)
			if _, err = h.Handshake(proxyApp); err != nil {
				t.Log(err)
			}
		})
	}
}

type badApp struct {
	abci.BaseApplication
	numBlocks           byte
	height              byte
	allHashesAreWrong   bool
	onlyLastHashIsWrong bool
}

func (app *badApp) Commit() abci.ResponseCommit {
	app.height++
	if app.onlyLastHashIsWrong {
		if app.height == app.numBlocks {
			return abci.ResponseCommit{Data: tmrand.Bytes(32)}
		}
		return abci.ResponseCommit{
			Data: []byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
				0, 0, 0, 0, 0, 0, 0, 0, app.height},
		}
	} else if app.allHashesAreWrong {
		return abci.ResponseCommit{Data: tmrand.Bytes(32)}
	}

	panic("either allHashesAreWrong or onlyLastHashIsWrong must be set")
}

//--------------------------
// utils for making blocks

func makeBlockchainFromWAL(wal WAL, genDoc *types.GenesisDoc) ([]*types.Block, []*types.Commit, error) {
	var height int64

	// Search for height marker
	gr, found, err := wal.SearchForEndHeight(height, &WALSearchOptions{})
	if err != nil {
		return nil, nil, err
	}
	if !found {
		return nil, nil, fmt.Errorf("wal does not contain height %d", height)
	}
	defer gr.Close()

	// log.Notice("Build a blockchain by reading from the WAL")

	var (
		blocks          []*types.Block
		commits         []*types.Commit
		thisBlockParts  *types.PartSet
		thisBlockCommit *types.Commit
	)

	dec := NewWALDecoder(gr)
	for {
		msg, err := dec.Decode()
		if err == io.EOF {
			break
		} else if err != nil {
			return nil, nil, err
		}

		piece := readPieceFromWAL(msg)
		if piece == nil {
			continue
		}

		switch p := piece.(type) {
		case EndHeightMessage:
			// if its not the first one, we have a full block
			if thisBlockParts != nil {
				var pbb = new(tmproto.Block)
				bz, err := ioutil.ReadAll(thisBlockParts.GetReader())
				if err != nil {
					panic(err)
				}
				err = proto.Unmarshal(bz, pbb)
				if err != nil {
					panic(err)
				}
				block, err := types.BlockFromProto(pbb)
				if err != nil {
					panic(err)
				}

				if block.Height != height+1 {
					panic(fmt.Sprintf("read bad block from wal. got height %d, expected %d", block.Height, height+1))
				}
				commitHeight := thisBlockCommit.Height
				if commitHeight != height+1 {
					panic(fmt.Sprintf("commit doesnt match. got height %d, expected %d", commitHeight, height+1))
				}
				blocks = append(blocks, block)
				commits = append(commits, thisBlockCommit)
				height++
			}
		case *types.PartSetHeader:
			thisBlockParts = types.NewPartSetFromHeader(*p)
		case *types.Part:
			_, err := thisBlockParts.AddPart(p)
			if err != nil {
				return nil, nil, err
			}
		case *types.Vote:
			if p.Type == tmproto.PrecommitType {
				// previous block, needed to detemine StateID
				var stateID types.StateID
				if len(blocks) >= 1 {
					prevBlock := blocks[len(blocks)-1]
					stateID = types.StateID{Height: prevBlock.Height, LastAppHash: prevBlock.AppHash}
				} else {
					stateID = types.StateID{Height: genDoc.InitialHeight, LastAppHash: genDoc.AppHash}
				}

				thisBlockCommit = types.NewCommit(p.Height, p.Round,
					p.BlockID, stateID, crypto.RandQuorumHash(), p.BlockSignature, p.StateSignature)
			}
		}
	}
	// grab the last block too
	bz, err := ioutil.ReadAll(thisBlockParts.GetReader())
	if err != nil {
		panic(err)
	}
	var pbb = new(tmproto.Block)
	err = proto.Unmarshal(bz, pbb)
	if err != nil {
		panic(err)
	}
	block, err := types.BlockFromProto(pbb)
	if err != nil {
		panic(err)
	}
	if block.Height != height+1 {
		panic(
			fmt.Sprintf(
				"read bad block from wal. got height %d, expected %d",
				block.Height,
				height+1,
			),
		)
	}
	commitHeight := thisBlockCommit.Height
	if commitHeight != height+1 {
		panic(
			fmt.Sprintf("commit doesnt match. got height %d, expected %d", commitHeight, height+1),
		)
	}
	blocks = append(blocks, block)
	commits = append(commits, thisBlockCommit)
	return blocks, commits, nil
}

func readPieceFromWAL(msg *TimedWALMessage) interface{} {
	// for logging
	switch m := msg.Msg.(type) {
	case msgInfo:
		switch msg := m.Msg.(type) {
		case *ProposalMessage:
			return &msg.Proposal.BlockID.PartSetHeader
		case *BlockPartMessage:
			return msg.Part
		case *VoteMessage:
			return msg.Vote
		}
	case EndHeightMessage:
		return m
	}

	return nil
}

// fresh state and mock store
func stateAndStore(
	cfg *config.Config,
	pubKey crypto.PubKey,
	appVersion uint64) (dbm.DB, sm.State, *mockBlockStore) {
	stateDB := dbm.NewMemDB()
	stateStore := sm.NewStore(stateDB)
	state, _ := sm.MakeGenesisStateFromFile(cfg.GenesisFile())
	state.Version.Consensus.App = appVersion
	store := newMockBlockStore(cfg, state.ConsensusParams)
	if err := stateStore.Save(state); err != nil {
		panic(err)
	}
	return stateDB, state, store
}

//----------------------------------
// mock block store

type mockBlockStore struct {
	cfg                   *config.Config
	params                types.ConsensusParams
	chain                 []*types.Block
	commits               []*types.Commit
	base                  int64
	coreChainLockedHeight uint32
}

// TODO: NewBlockStore(db.NewMemDB) ...
func newMockBlockStore(cfg *config.Config, params types.ConsensusParams) *mockBlockStore {
	return &mockBlockStore{cfg, params, nil, nil, 0, 1}
}

func (bs *mockBlockStore) Height() int64                 { return int64(len(bs.chain)) }
func (bs *mockBlockStore) CoreChainLockedHeight() uint32 { return bs.coreChainLockedHeight }
func (bs *mockBlockStore) Base() int64                   { return bs.base }

func (bs *mockBlockStore) Size() int64                         { return bs.Height() - bs.Base() + 1 }
func (bs *mockBlockStore) LoadBaseMeta() *types.BlockMeta      { return bs.LoadBlockMeta(bs.base) }
func (bs *mockBlockStore) LoadBlock(height int64) *types.Block { return bs.chain[height-1] }
func (bs *mockBlockStore) LoadBlockByHash(hash []byte) *types.Block {
	return bs.chain[int64(len(bs.chain))-1]
}
func (bs *mockBlockStore) LoadBlockMeta(height int64) *types.BlockMeta {
	block := bs.chain[height-1]
	return &types.BlockMeta{
		BlockID: types.BlockID{
			Hash:          block.Hash(),
			PartSetHeader: block.MakePartSet(types.BlockPartSizeBytes).Header(),
		},
		Header: block.Header,
	}
}
func (bs *mockBlockStore) LoadBlockPart(height int64, index int) *types.Part { return nil }

func (bs *mockBlockStore) SaveBlock(
	block *types.Block,
	blockParts *types.PartSet,
	seenCommit *types.Commit,
) {
}
func (bs *mockBlockStore) LoadBlockCommit(height int64) *types.Commit {
	return bs.commits[height-1]
}
func (bs *mockBlockStore) LoadSeenCommit() *types.Commit {
	return bs.commits[len(bs.commits)-1]
}

func (bs *mockBlockStore) PruneBlocks(height int64) (uint64, error) {
	pruned := uint64(0)
	for i := int64(0); i < height-1; i++ {
		bs.chain[i] = nil
		bs.commits[i] = nil
		pruned++
	}
	bs.base = height
	return pruned, nil
}

//---------------------------------------
// Test handshake/init chain

func TestHandshakeUpdatesValidators(t *testing.T) {
	cfg, err := ResetConfig("handshake_test_")
	require.NoError(t, err)
	defer os.RemoveAll(cfg.RootDir)
	privVal, err := privval.LoadFilePV(cfg.PrivValidator.KeyFile(), cfg.PrivValidator.StateFile())
	require.NoError(t, err)

	val, _ := randValidator()
	randQuorumHash, err := privVal.GetFirstQuorumHash(context.Background())
	require.NoError(t, err)
	vals := types.NewValidatorSet(
		[]*types.Validator{val},
		val.PubKey,
		btcjson.LLMQType_5_60,
		randQuorumHash,
		true,
	)
	abciValidatorSetUpdates := types.TM2PB.ValidatorUpdates(vals)
	app := &initChainApp{vals: &abciValidatorSetUpdates}
	clientCreator := abciclient.NewLocalCreator(app)

	pubKey, err := privVal.GetPubKey(context.Background(), randQuorumHash)
	require.NoError(t, err)
	proTxHash, err := privVal.GetProTxHash(context.Background())
	require.NoError(t, err)
	stateDB, state, store := stateAndStore(cfg, pubKey, 0x0)
	stateStore := sm.NewStore(stateDB)

	oldValProTxHash := state.Validators.Validators[0].ProTxHash

	// now start the app using the handshake - it should sync
	genDoc, _ := sm.MakeGenesisDocFromFile(cfg.GenesisFile())
	handshaker := NewHandshaker(
		stateStore,
		state,
		store,
		genDoc,
		proTxHash,
		cfg.Consensus.AppHashSize,
	)
	proxyApp := proxy.NewAppConns(clientCreator)
	if err := proxyApp.Start(); err != nil {
		t.Fatalf("Error starting proxy app connections: %v", err)
	}
	t.Cleanup(func() {
		if err := proxyApp.Stop(); err != nil {
			t.Error(err)
		}
	})
	if _, err := handshaker.Handshake(proxyApp); err != nil {
		t.Fatalf("Error on abci handshake: %v", err)
	}
	// reload the state, check the validator set was updated
	state, err = stateStore.Load()
	require.NoError(t, err)

	newValProTxHash := state.Validators.Validators[0].ProTxHash
	expectValProTxHash := val.ProTxHash
	assert.NotEqual(t, oldValProTxHash, newValProTxHash)
	assert.Equal(t, newValProTxHash, expectValProTxHash)
}

func TestHandshakeInitialCoreLockHeight(t *testing.T) {
	const InitialCoreHeight uint32 = 12345
	config, err := ResetConfig("handshake_test_initial_core_lock_height")
	require.NoError(t, err)
	defer os.RemoveAll(config.RootDir)

	privVal, err := privval.LoadFilePV(config.PrivValidator.KeyFile(), config.PrivValidator.StateFile())
	require.NoError(t, err)

	randQuorumHash, err := privVal.GetFirstQuorumHash(context.TODO())
	require.NoError(t, err)

	app := &initChainApp{initialCoreHeight: InitialCoreHeight}
	clientCreator := abciclient.NewLocalCreator(app)
	require.NotNil(t, clientCreator)
	proxyApp := proxy.NewAppConns(clientCreator)
	require.NotNil(t, proxyApp)

	pubKey, err := privVal.GetPubKey(context.TODO(), randQuorumHash)
	require.NoError(t, err)
	proTxHash, err := privVal.GetProTxHash(context.TODO())
	require.NoError(t, err)
	stateDB, state, store := stateAndStore(config, pubKey, 0x0)
	stateStore := sm.NewStore(stateDB)

	// now start the app using the handshake - it should sync
	genDoc, _ := sm.MakeGenesisDocFromFile(config.GenesisFile())
	handshaker := NewHandshaker(
		stateStore,
		state,
		store,
		genDoc,
		proTxHash,
		config.Consensus.AppHashSize,
	)

	if err := proxyApp.Start(); err != nil {
		t.Fatalf("Error starting proxy app connections: %v", err)
	}
	t.Cleanup(func() {
		if err := proxyApp.Stop(); err != nil {
			t.Error(err)
		}
	})
	if _, err := handshaker.Handshake(proxyApp); err != nil {
		t.Fatalf("Error on abci handshake: %v", err)
	}

	// reload the state, check the validator set was updated
	state, err = stateStore.Load()
	require.NoError(t, err)
	assert.Equal(t, InitialCoreHeight, state.LastCoreChainLockedBlockHeight)
	assert.Equal(t, InitialCoreHeight, handshaker.initialState.LastCoreChainLockedBlockHeight)
}

// returns the vals on InitChain
type initChainApp struct {
	abci.BaseApplication
	vals              *abci.ValidatorSetUpdate
	initialCoreHeight uint32
}

func (ica *initChainApp) InitChain(req abci.RequestInitChain) abci.ResponseInitChain {
	resp := abci.ResponseInitChain{
		InitialCoreHeight: ica.initialCoreHeight,
	}
	if ica.vals != nil {
		resp.ValidatorSetUpdate = *ica.vals
	}
	return resp
}

func randValidator() (*types.Validator, types.PrivValidator) {
	quorumHash := crypto.RandQuorumHash()
	privVal := types.NewMockPVForQuorum(quorumHash)
	proTxHash, _ := privVal.GetProTxHash(context.Background())
	pubKey, _ := privVal.GetPubKey(context.Background(), quorumHash)
	val := types.NewValidatorDefaultVotingPower(pubKey, proTxHash)
	return val, privVal
}