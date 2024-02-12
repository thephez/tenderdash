package abciclient_test

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	abciclient "github.com/dashpay/tenderdash/abci/client"
	"github.com/dashpay/tenderdash/abci/server"
	"github.com/dashpay/tenderdash/abci/types"
	"github.com/dashpay/tenderdash/abci/types/mocks"
	"github.com/dashpay/tenderdash/libs/log"
)

// TestRouting tests the RoutedClient.
//
// Given 3 clients: defaultApp, consensusApp and queryApp:
// * when a request of type Info is made, it should be routed to defaultApp
// * when a request of type FinalizeBlock is made, it should be first routed to queryApp, then to consensusApp
// * when a request of type CheckTx is made, it should be routed to queryApp
// * when a request of type PrepareProposal is made, it should be routed to to consensusApp
func TestRouting(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// infoMtx blocks Info until we finish the test
	var infoMtx sync.Mutex
	infoMtx.Lock()
	infoExecuted := false

	logger := log.NewTestingLogger(t)

	defaultApp, defaultSocket := startApp(ctx, t, logger, "default")
	defer defaultApp.AssertExpectations(t)

	defaultApp.On("Info", mock.Anything, mock.Anything).Return(&types.ResponseInfo{
		Data: "info",
	}, nil).Run(func(args mock.Arguments) {
		t.Log("Info: before lock")
		infoMtx.Lock()
		defer infoMtx.Unlock()
		t.Log("Info: after lock")
		infoExecuted = true
	}).Once()

	queryApp, querySocket := startApp(ctx, t, logger, "query")
	defer queryApp.AssertExpectations(t)
	queryApp.On("CheckTx", mock.Anything, mock.Anything).Return(&types.ResponseCheckTx{
		Priority: 1,
	}, nil).Once()
	queryApp.On("FinalizeBlock", mock.Anything, mock.Anything).Return(&types.ResponseFinalizeBlock{}, nil).Once()

	consensusApp, consensusSocket := startApp(ctx, t, logger, "consensus")
	defer consensusApp.AssertExpectations(t)
	consensusApp.On("PrepareProposal", mock.Anything, mock.Anything).Return(&types.ResponsePrepareProposal{
		AppHash: []byte("apphash"),
	}, nil).Once()
	consensusApp.On("FinalizeBlock", mock.Anything, mock.Anything).Return(&types.ResponseFinalizeBlock{
		RetainHeight: 1,
	}, nil).Once()

	addr := fmt.Sprintf("CheckTx:socket:%s", querySocket) +
		fmt.Sprintf(",FinalizeBlock:socket:%s,FinalizeBlock:socket:%s", querySocket, consensusSocket) +
		fmt.Sprintf(",PrepareProposal:socket:%s", consensusSocket) +
		fmt.Sprintf(",*:socket:%s", defaultSocket)

	logger.Info("configuring routed abci client with address", "addr", addr)
	routedClient, err := abciclient.NewRoutedClientWithAddr(logger, addr, true)
	assert.NoError(t, err)
	err = routedClient.Start(ctx)
	assert.NoError(t, err)

	// Test routing
	wg := sync.WaitGroup{}

	// Info is called from separate thread, as we want it to block
	// to see if we can execute other calls (on other clients) without blocking
	wg.Add(1)
	go func() {
		// info is locked, so it should finish last
		_, err := routedClient.Info(ctx, &types.RequestInfo{})
		require.NoError(t, err)
		wg.Done()
	}()

	// CheckTx
	_, err = routedClient.CheckTx(ctx, &types.RequestCheckTx{})
	assert.NoError(t, err)

	// FinalizeBlock
	_, err = routedClient.FinalizeBlock(ctx, &types.RequestFinalizeBlock{})
	assert.NoError(t, err)

	// PrepareProposal
	_, err = routedClient.PrepareProposal(ctx, &types.RequestPrepareProposal{})
	assert.NoError(t, err)

	// unlock info
	assert.False(t, infoExecuted)
	infoMtx.Unlock()
	wg.Wait()
	assert.True(t, infoExecuted)
}

func startApp(ctx context.Context, t *testing.T, logger log.Logger, id string) (*mocks.Application, string) {
	app := mocks.NewApplication(t)
	defer app.AssertExpectations(t)

	addr := fmt.Sprintf("unix://%s/%s", t.TempDir(), "/socket."+id)

	server, err := server.NewServer(logger, addr, "socket", app)
	require.NoError(t, err)
	err = server.Start(ctx)
	require.NoError(t, err)

	return app, addr
}
