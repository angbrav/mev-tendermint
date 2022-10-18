package v0

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	mrand "math/rand"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/gogo/protobuf/proto"
	gogotypes "github.com/gogo/protobuf/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	abciclient "github.com/tendermint/tendermint/abci/client"
	abciclimocks "github.com/tendermint/tendermint/abci/client/mocks"
	"github.com/tendermint/tendermint/abci/example/kvstore"
	abciserver "github.com/tendermint/tendermint/abci/server"
	abci "github.com/tendermint/tendermint/abci/types"
	"github.com/tendermint/tendermint/config"
	"github.com/tendermint/tendermint/libs/log"
	tmrand "github.com/tendermint/tendermint/libs/rand"
	"github.com/tendermint/tendermint/libs/service"
	"github.com/tendermint/tendermint/mempool"
	"github.com/tendermint/tendermint/proxy"
	"github.com/tendermint/tendermint/types"
)

// A cleanupFunc cleans up any config / test files created for a particular
// test.
type cleanupFunc func()

type testBundleInfo struct {
	BundleSize    int64
	DesiredHeight int64
	BundleID      int64
	PeerID        uint16
}

var (
	ZeroedTxInfoForSidecar = mempool.TxInfo{DesiredHeight: 1, BundleID: 0, BundleOrder: 0, BundleSize: 1}
)

func newMempoolWithAppMock(cc proxy.ClientCreator, client abciclient.Client) (*CListMempool, cleanupFunc, error) {
	conf := config.ResetTestRoot("mempool_test")

	mp, cu := newMempoolWithAppAndConfigMock(cc, conf, client)
	return mp, cu, nil
}

func newMempoolWithAppAndConfigMock(cc proxy.ClientCreator,
	cfg *config.Config,
	client abciclient.Client) (*CListMempool, cleanupFunc) {
	appConnMem := client
	appConnMem.SetLogger(log.TestingLogger().With("module", "abci-client", "connection", "mempool"))
	err := appConnMem.Start()
	if err != nil {
		panic(err)
	}

	mp := NewCListMempool(cfg.Mempool, appConnMem, 0)
	mp.SetLogger(log.TestingLogger())

	return mp, func() { os.RemoveAll(cfg.RootDir) }
}

func newMempoolWithApp(cc proxy.ClientCreator) (*CListMempool, *CListPriorityTxSidecar, cleanupFunc) {
	conf := config.ResetTestRoot("mempool_test")

	mp, sc, cu := newMempoolWithAppAndConfig(cc, conf)
	return mp, sc, cu
}

func newMempoolWithAppAndConfig(cc proxy.ClientCreator, cfg *config.Config) (*CListMempool, *CListPriorityTxSidecar, cleanupFunc) {
	appConnMem, _ := cc.NewABCIClient()
	appConnMem.SetLogger(log.TestingLogger().With("module", "abci-client", "connection", "mempool"))
	err := appConnMem.Start()
	if err != nil {
		panic(err)
	}

	mp := NewCListMempool(cfg.Mempool, appConnMem, 0)
	mp.SetLogger(log.TestingLogger())
	sidecar := NewCListSidecar(0, log.NewNopLogger(), mempool.NopMetrics())

	return mp, sidecar, func() { os.RemoveAll(cfg.RootDir) }
}

func ensureNoFire(t *testing.T, ch <-chan struct{}, timeoutMS int) {
	timer := time.NewTimer(time.Duration(timeoutMS) * time.Millisecond)
	select {
	case <-ch:
		t.Fatal("Expected not to fire")
	case <-timer.C:
	}
}

func ensureFire(t *testing.T, ch <-chan struct{}, timeoutMS int) {
	timer := time.NewTimer(time.Duration(timeoutMS) * time.Millisecond)
	select {
	case <-ch:
	case <-timer.C:
		t.Fatal("Expected to fire")
	}
}

func checkTxs(t *testing.T, mp mempool.Mempool, count int, peerID uint16, sidecar mempool.PriorityTxSidecar, addToSidecar bool) types.Txs {
	txs := make(types.Txs, count)
	txInfo := mempool.TxInfo{SenderID: peerID}
	for i := 0; i < count; i++ {
		txBytes := make([]byte, 20)
		txs[i] = txBytes
		_, err := rand.Read(txBytes)
		if err != nil {
			t.Error(err)
		}
		if err := mp.CheckTx(txBytes, nil, txInfo); err != nil {
			// Skip invalid txs.
			// TestMempoolFilters will fail otherwise. It asserts a number of txs
			// returned.
			if mempool.IsPreCheckError(err) {
				continue
			}
			t.Fatalf("CheckTx failed: %v while checking #%d tx", err, i)
		}
		if addToSidecar {
			if err := sidecar.AddTx(txBytes, txInfo); err != nil {
				t.Error(err)
			}
		}
	}
	return txs
}

func addNumBundlesToSidecar(t *testing.T, sidecar mempool.PriorityTxSidecar, numBundles int, bundleSize int64, peerID uint16) types.Txs {
	totalTxsCount := 0
	txs := make(types.Txs, 0)
	for i := 0; i < numBundles; i++ {
		totalTxsCount += int(bundleSize)
		newTxs := createSidecarBundleAndTxs(t, sidecar, testBundleInfo{BundleSize: bundleSize,
			PeerID: mempool.UnknownPeerID, DesiredHeight: sidecar.HeightForFiringAuction(), BundleID: int64(i)})
		txs = append(txs, newTxs...)
	}
	return txs
}

func addSpecificTxsToSidecarOneBundle(t *testing.T, sidecar mempool.PriorityTxSidecar, txs types.Txs, peerID uint16) types.Txs {

	bInfo := testBundleInfo{BundleSize: int64(len(txs)), PeerID: peerID, DesiredHeight: sidecar.HeightForFiringAuction(), BundleID: 0}
	for i := 0; i < len(txs); i++ {
		err := sidecar.AddTx(txs[i], mempool.TxInfo{SenderID: bInfo.PeerID, BundleSize: bInfo.BundleSize,
			BundleID: bInfo.BundleID, DesiredHeight: bInfo.DesiredHeight, BundleOrder: int64(i)})
		if err != nil {
			t.Error(err)
		}
	}
	return txs
}

func addNumTxsToSidecarOneBundle(t *testing.T, sidecar mempool.PriorityTxSidecar, numTxs int, peerID uint16) types.Txs {

	txs := make(types.Txs, numTxs)
	bInfo := testBundleInfo{BundleSize: int64(numTxs), PeerID: peerID, DesiredHeight: sidecar.HeightForFiringAuction(), BundleID: 0}
	for i := 0; i < numTxs; i++ {
		txBytes := addTxToSidecar(t, sidecar, bInfo, int64(i))
		txs = append(txs, txBytes)
	}
	return txs
}

func addTxToSidecar(t *testing.T, sidecar mempool.PriorityTxSidecar, bInfo testBundleInfo, bundleOrder int64) types.Tx {
	txInfo := mempool.TxInfo{SenderID: bInfo.PeerID, BundleSize: bInfo.BundleSize,
		BundleID: bInfo.BundleID, DesiredHeight: bInfo.DesiredHeight, BundleOrder: bundleOrder}
	txBytes := make([]byte, 20)
	_, err := rand.Read(txBytes)
	if err != nil {
		t.Error(err)
	}
	if err := sidecar.AddTx(txBytes, txInfo); err != nil {
		t.Error(err)
	}
	return txBytes
}

func createSidecarBundleAndTxs(t *testing.T, sidecar mempool.PriorityTxSidecar, bInfo testBundleInfo) types.Txs {
	txs := make(types.Txs, bInfo.BundleSize)
	for i := 0; i < int(bInfo.BundleSize); i++ {
		txBytes := addTxToSidecar(t, sidecar, bInfo, int64(i))
		txs[i] = txBytes
	}
	return txs
}

func addBundlesToSidecar(t *testing.T, sidecar mempool.PriorityTxSidecar, bundles []testBundleInfo, peerID uint16) {
	for _, bundle := range bundles {
		// createSidecarBundleWithTxs(t, sidecar, bundle.BundleSize, peerID, bundle.BundleID, bundle.DesiredHeight)
		createSidecarBundleAndTxs(t, sidecar, bundle)
	}
}

func TestReapMaxBytesMaxGas(t *testing.T) {
	app := kvstore.NewApplication()
	cc := proxy.NewLocalClientCreator(app)
	mp, _, cleanup := newMempoolWithApp(cc)
	defer cleanup()

	// Ensure gas calculation behaves as expected
	checkTxs(t, mp, 1, mempool.UnknownPeerID, nil, false)
	tx0 := mp.TxsFront().Value.(*mempool.MempoolTx)
	// assert that kv store has gas wanted = 1.
	require.Equal(t, app.CheckTx(abci.RequestCheckTx{Tx: tx0.Tx}).GasWanted, int64(1), "KVStore had a gas value neq to 1")
	require.Equal(t, tx0.GasWanted, int64(1), "transactions gas was set incorrectly")
	// ensure each tx is 20 bytes long
	require.Equal(t, len(tx0.Tx), 20, "Tx is longer than 20 bytes")
	mp.Flush()

	// each table driven test creates numTxsToCreate txs with checkTx, and at the end clears all remaining txs.
	// each tx has 20 bytes
	tests := []struct {
		numTxsToCreate int
		maxBytes       int64
		maxGas         int64
		expectedNumTxs int
	}{
		{20, -1, -1, 20},
		{20, -1, 0, 0},
		{20, -1, 10, 10},
		{20, -1, 30, 20},
		{20, 0, -1, 0},
		{20, 0, 10, 0},
		{20, 10, 10, 0},
		{20, 24, 10, 1},
		{20, 240, 5, 5},
		{20, 240, -1, 10},
		{20, 240, 10, 10},
		{20, 240, 15, 10},
		{20, 20000, -1, 20},
		{20, 20000, 5, 5},
		{20, 20000, 30, 20},
	}
	for tcIndex, tt := range tests {
		checkTxs(t, mp, tt.numTxsToCreate, mempool.UnknownPeerID, nil, false)
		got := mp.ReapMaxBytesMaxGas(tt.maxBytes, tt.maxGas, nil)
		assert.Equal(t, tt.expectedNumTxs, len(got), "Got %d txs, expected %d, tc #%d",
			len(got), tt.expectedNumTxs, tcIndex)
		mp.Flush()
	}
}

func TestBasicAddMultipleBundles(t *testing.T) {
	app := kvstore.NewApplication()
	cc := proxy.NewLocalClientCreator(app)
	_, sidecar, cleanup := newMempoolWithApp(cc)
	defer cleanup()

	tests := []struct {
		numBundlesTxsToCreate int
	}{
		{0},
		{1},
		{5},
		{0},
		{100},
	}
	for tcIndex, tt := range tests {
		fmt.Println("Num bundles to create: ", tt.numBundlesTxsToCreate)
		addNumBundlesToSidecar(t, sidecar, tt.numBundlesTxsToCreate, 10, mempool.UnknownPeerID)
		sidecar.ReapMaxTxs()
		assert.Equal(t, tt.numBundlesTxsToCreate, sidecar.NumBundles(), "Got %d bundles, expected %d, tc #%d",
			sidecar.NumBundles(), tt.numBundlesTxsToCreate, tcIndex)
		sidecar.Flush()
	}
}

func TestSpecificAddTxsToMultipleBundles(t *testing.T) {
	app := kvstore.NewApplication()
	cc := proxy.NewLocalClientCreator(app)
	_, sidecar, cleanup := newMempoolWithApp(cc)
	defer cleanup()

	// only one since no txs in first
	{
		// bundleSize, bundleHeight, bundleID, peerID
		bundles := []testBundleInfo{
			{0, 1, 0, 0},
			{5, 1, 1, 0},
		}
		addBundlesToSidecar(t, sidecar, bundles, mempool.UnknownPeerID)
		assert.Equal(t, 1, sidecar.NumBundles(), "Got %d bundles, expected %d",
			sidecar.NumBundles(), 1)
		sidecar.Flush()
	}

	// three bundles
	{
		// bundleSize, bundleHeight, bundleID, peerID
		bundles := []testBundleInfo{
			{5, 1, 0, 0},
			{5, 5, 1, 0},
			{5, 10, 1, 0},
		}
		addBundlesToSidecar(t, sidecar, bundles, mempool.UnknownPeerID)
		assert.Equal(t, 3, sidecar.NumBundles(), "Got %d bundles, expected %d",
			sidecar.NumBundles(), 1)
		sidecar.Flush()
	}

	// only one bundle since we already have all these bundleOrders
	{
		// bundleSize, bundleHeight, bundleID
		bundles := []testBundleInfo{
			{5, 1, 0, 0},
			{5, 1, 0, 0},
			{5, 1, 0, 0},
		}
		addBundlesToSidecar(t, sidecar, bundles, mempool.UnknownPeerID)
		assert.Equal(t, 1, sidecar.NumBundles(), "Got %d bundles, expected %d",
			sidecar.NumBundles(), 1)
		sidecar.Flush()
	}

	// only one bundle since we already have all these bundleOrders
	{
		// bundleSize, bundleHeight, BundleID
		bundles := []testBundleInfo{
			{5, 1, 0, 0},
			{5, 1, 3, 0},
			{5, 1, 5, 0},
		}
		addBundlesToSidecar(t, sidecar, bundles, mempool.UnknownPeerID)
		assert.Equal(t, 3, sidecar.NumBundles(), "Got %d bundles, expected %d",
			sidecar.NumBundles(), 3)
		sidecar.Flush()
	}
}

// TODO: shorten
func TestReapSidecarWithTxsOutOfOrder(t *testing.T) {
	app := kvstore.NewApplication()
	cc := proxy.NewLocalClientCreator(app)
	_, sidecar, cleanup := newMempoolWithApp(cc)
	defer cleanup()

	// 1. Inserted out of order, but sequential, but bundleSize 1, so should get one tx
	{
		bInfo := testBundleInfo{
			BundleSize:    1,
			PeerID:        mempool.UnknownPeerID,
			DesiredHeight: 1,
			BundleID:      0,
		}
		var bundleOrder int64 = 1
		addTxToSidecar(t, sidecar, bInfo, bundleOrder)

		bInfo = testBundleInfo{
			BundleSize:    1,
			PeerID:        mempool.UnknownPeerID,
			DesiredHeight: 1,
			BundleID:      0,
		}
		bundleOrder = 0
		addTxToSidecar(t, sidecar, bInfo, bundleOrder)

		sidecar.PrettyPrintBundles()

		txs := sidecar.ReapMaxTxs()
		assert.Equal(t, 1, len(txs), "Got %d txs, expected %d",
			len(txs), 1)

		sidecar.Flush()
	}

	// 2. Same as before but now size is open to 2, so expect 2
	{
		bInfo := testBundleInfo{
			BundleSize:    2,
			PeerID:        mempool.UnknownPeerID,
			DesiredHeight: 1,
			BundleID:      0,
		}
		var bundleOrder int64 = 1
		addTxToSidecar(t, sidecar, bInfo, bundleOrder)

		bInfo = testBundleInfo{
			BundleSize:    2,
			PeerID:        mempool.UnknownPeerID,
			DesiredHeight: 1,
			BundleID:      0,
		}
		bundleOrder = 0
		addTxToSidecar(t, sidecar, bInfo, bundleOrder)

		txs := sidecar.ReapMaxTxs()
		assert.Equal(t, 2, len(txs), "Got %d txs, expected %d",
			len(txs), 2)

		sidecar.Flush()
	}

	// 3. Insert a bundle out of order and non sequential, so nothing should happen
	{
		bInfo := testBundleInfo{
			BundleSize:    5,
			PeerID:        mempool.UnknownPeerID,
			DesiredHeight: 1,
			BundleID:      0,
		}
		var bundleOrder int64 = 3
		addTxToSidecar(t, sidecar, bInfo, bundleOrder)

		bInfo = testBundleInfo{
			BundleSize:    5,
			PeerID:        mempool.UnknownPeerID,
			DesiredHeight: 1,
			BundleID:      0,
		}
		bundleOrder = 1
		addTxToSidecar(t, sidecar, bInfo, bundleOrder)

		txs := sidecar.ReapMaxTxs()
		assert.Equal(t, 0, len(txs), "Got %d txs, expected %d",
			len(txs), 0)

		sidecar.Flush()
	}

	// 4. Insert three successful bundles out of order
	//nolint:dupl
	{
		bInfo := testBundleInfo{
			BundleSize:    3,
			PeerID:        mempool.UnknownPeerID,
			DesiredHeight: 1,
			BundleID:      2,
		}
		var bundleOrder int64 = 2
		addTxToSidecar(t, sidecar, bInfo, bundleOrder)

		bInfo = testBundleInfo{
			BundleSize:    3,
			PeerID:        mempool.UnknownPeerID,
			DesiredHeight: 1,
			BundleID:      2,
		}
		bundleOrder = 0
		addTxToSidecar(t, sidecar, bInfo, bundleOrder)

		bInfo = testBundleInfo{
			BundleSize:    3,
			PeerID:        mempool.UnknownPeerID,
			DesiredHeight: 1,
			BundleID:      2,
		}
		bundleOrder = 1
		addTxToSidecar(t, sidecar, bInfo, bundleOrder)

		// ============

		bInfo = testBundleInfo{
			BundleSize:    2,
			PeerID:        mempool.UnknownPeerID,
			DesiredHeight: 1,
			BundleID:      0,
		}
		bundleOrder = 1
		addTxToSidecar(t, sidecar, bInfo, bundleOrder)

		bInfo = testBundleInfo{
			BundleSize:    2,
			PeerID:        mempool.UnknownPeerID,
			DesiredHeight: 1,
			BundleID:      0,
		}
		bundleOrder = 0
		addTxToSidecar(t, sidecar, bInfo, bundleOrder)

		// ============

		bInfo = testBundleInfo{
			BundleSize:    2,
			PeerID:        mempool.UnknownPeerID,
			DesiredHeight: 1,
			BundleID:      1,
		}
		bundleOrder = 1
		addTxToSidecar(t, sidecar, bInfo, bundleOrder)

		bInfo = testBundleInfo{
			BundleSize:    2,
			PeerID:        mempool.UnknownPeerID,
			DesiredHeight: 1,
			BundleID:      1,
		}
		bundleOrder = 0
		addTxToSidecar(t, sidecar, bInfo, bundleOrder)

		txs := sidecar.ReapMaxTxs()
		assert.Equal(t, 7, len(txs), "Got %d txs, expected %d",
			len(txs), 7)
		sidecar.PrettyPrintBundles()

		fmt.Println("TXS FROM REAP ----------")
		for _, memTx := range txs {
			fmt.Println(memTx.Tx.String())
		}
		fmt.Println("----------")

		sidecar.Flush()
	}

	// 5. Multiple unsuccessful bundles, nothing reaped
	//nolint:dupl
	{
		// size not filled
		bInfo := testBundleInfo{
			BundleSize:    3,
			PeerID:        mempool.UnknownPeerID,
			DesiredHeight: 1,
			BundleID:      2,
		}
		var bundleOrder int64
		addTxToSidecar(t, sidecar, bInfo, bundleOrder)

		bInfo = testBundleInfo{
			BundleSize:    3,
			PeerID:        mempool.UnknownPeerID,
			DesiredHeight: 1,
			BundleID:      2,
		}
		bundleOrder = 1
		addTxToSidecar(t, sidecar, bInfo, bundleOrder)

		// ============

		// wrong orders
		bInfo = testBundleInfo{
			BundleSize:    3,
			PeerID:        mempool.UnknownPeerID,
			DesiredHeight: 1,
			BundleID:      0,
		}
		bundleOrder = 2
		addTxToSidecar(t, sidecar, bInfo, bundleOrder)

		bInfo = testBundleInfo{
			BundleSize:    3,
			PeerID:        mempool.UnknownPeerID,
			DesiredHeight: 1,
			BundleID:      0,
		}
		bundleOrder = 0
		addTxToSidecar(t, sidecar, bInfo, bundleOrder)

		bInfo = testBundleInfo{
			BundleSize:    3,
			PeerID:        mempool.UnknownPeerID,
			DesiredHeight: 1,
			BundleID:      0,
		}
		bundleOrder = 3
		addTxToSidecar(t, sidecar, bInfo, bundleOrder)

		// ============

		// wrong heights
		bInfo = testBundleInfo{
			BundleSize:    2,
			PeerID:        mempool.UnknownPeerID,
			DesiredHeight: 2,
			BundleID:      1,
		}
		bundleOrder = 1
		addTxToSidecar(t, sidecar, bInfo, bundleOrder)

		bInfo = testBundleInfo{
			BundleSize:    2,
			PeerID:        mempool.UnknownPeerID,
			DesiredHeight: 0,
			BundleID:      1,
		}
		bundleOrder = 0
		addTxToSidecar(t, sidecar, bInfo, bundleOrder)

		txs := sidecar.ReapMaxTxs()
		assert.Equal(t, 0, len(txs), "Got %d txs, expected %d",
			len(txs), 0)
		sidecar.PrettyPrintBundles()

		fmt.Println("TXS FROM REAP ----------")
		for _, memTx := range txs {
			fmt.Println(memTx.Tx.String())
		}
		fmt.Println("----------")

		sidecar.Flush()
	}

}

func TestMempoolFilters(t *testing.T) {
	app := kvstore.NewApplication()
	cc := proxy.NewLocalClientCreator(app)
	mp, _, cleanup := newMempoolWithApp(cc)
	defer cleanup()
	emptyTxArr := []types.Tx{[]byte{}}

	nopPreFilter := func(tx types.Tx) error { return nil }
	nopPostFilter := func(tx types.Tx, res *abci.ResponseCheckTx) error { return nil }

	// each table driven test creates numTxsToCreate txs with checkTx, and at the end clears all remaining txs.
	// each tx has 20 bytes
	tests := []struct {
		numTxsToCreate int
		preFilter      mempool.PreCheckFunc
		postFilter     mempool.PostCheckFunc
		expectedNumTxs int
	}{
		{10, nopPreFilter, nopPostFilter, 10},
		{10, mempool.PreCheckMaxBytes(10), nopPostFilter, 0},
		{10, mempool.PreCheckMaxBytes(22), nopPostFilter, 10},
		{10, nopPreFilter, mempool.PostCheckMaxGas(-1), 10},
		{10, nopPreFilter, mempool.PostCheckMaxGas(0), 0},
		{10, nopPreFilter, mempool.PostCheckMaxGas(1), 10},
		{10, nopPreFilter, mempool.PostCheckMaxGas(3000), 10},
		{10, mempool.PreCheckMaxBytes(10), mempool.PostCheckMaxGas(20), 0},
		{10, mempool.PreCheckMaxBytes(30), mempool.PostCheckMaxGas(20), 10},
		{10, mempool.PreCheckMaxBytes(22), mempool.PostCheckMaxGas(1), 10},
		{10, mempool.PreCheckMaxBytes(22), mempool.PostCheckMaxGas(0), 0},
	}
	for tcIndex, tt := range tests {
		err := mp.Update(1, emptyTxArr, abciResponses(len(emptyTxArr), abci.CodeTypeOK), tt.preFilter, tt.postFilter)
		require.NoError(t, err)
		checkTxs(t, mp, tt.numTxsToCreate, mempool.UnknownPeerID, nil, false)
		require.Equal(t, tt.expectedNumTxs, mp.Size(), "mempool had the incorrect size, on test case %d", tcIndex)
		mp.Flush()
	}
}

func TestMempoolUpdate(t *testing.T) {
	app := kvstore.NewApplication()
	cc := proxy.NewLocalClientCreator(app)
	mp, _, cleanup := newMempoolWithApp(cc)
	defer cleanup()

	// 1. Adds valid txs to the cache
	{
		err := mp.Update(1, []types.Tx{[]byte{0x01}}, abciResponses(1, abci.CodeTypeOK), nil, nil)
		require.NoError(t, err)
		err = mp.CheckTx([]byte{0x01}, nil, mempool.TxInfo{})
		if assert.Error(t, err) {
			assert.Equal(t, mempool.ErrTxInCache, err)
		}
	}

	// 2. Removes valid txs from the mempool
	{
		err := mp.CheckTx([]byte{0x02}, nil, mempool.TxInfo{})
		require.NoError(t, err)
		err = mp.Update(1, []types.Tx{[]byte{0x02}}, abciResponses(1, abci.CodeTypeOK), nil, nil)
		require.NoError(t, err)
		assert.Zero(t, mp.Size())
	}

	// 3. Removes invalid transactions from the cache and the mempool (if present)
	{
		err := mp.CheckTx([]byte{0x03}, nil, mempool.TxInfo{})
		require.NoError(t, err)
		err = mp.Update(1, []types.Tx{[]byte{0x03}}, abciResponses(1, 1), nil, nil)
		require.NoError(t, err)
		assert.Zero(t, mp.Size())

		err = mp.CheckTx([]byte{0x03}, nil, mempool.TxInfo{})
		require.NoError(t, err)
	}
}

func TestSidecarUpdate(t *testing.T) {
	app := kvstore.NewApplication()
	cc := proxy.NewLocalClientCreator(app)
	_, sidecar, cleanup := newMempoolWithApp(cc)
	defer cleanup()

	// 1. Flushes the sidecar
	{
		bInfo := testBundleInfo{
			BundleSize:    2,
			PeerID:        mempool.UnknownPeerID,
			DesiredHeight: 1,
			BundleID:      0,
		}
		var bundleOrder int64
		addTxToSidecar(t, sidecar, bInfo, bundleOrder)

		bInfo = testBundleInfo{
			BundleSize:    2,
			PeerID:        mempool.UnknownPeerID,
			DesiredHeight: 1,
			BundleID:      0,
		}
		bundleOrder = 1
		addTxToSidecar(t, sidecar, bInfo, bundleOrder)

		err := sidecar.Update(0, []types.Tx{[]byte{0x02}}, abciResponses(1, abci.CodeTypeOK))
		require.NoError(t, err)
		require.Equal(t, 2, sidecar.Size(), "foo with a newline should be written")
		err = sidecar.Update(1, []types.Tx{[]byte{0x02}}, abciResponses(1, abci.CodeTypeOK))
		require.NoError(t, err)
		require.Equal(t, 0, sidecar.Size(), "foo with a newline should be written")
	}
}

func TestMempoolUpdateDoesNotPanicWhenApplicationMissedTx(t *testing.T) {
	var callback abciclient.Callback
	mockClient := new(abciclimocks.Client)
	mockClient.On("Start").Return(nil)
	mockClient.On("SetLogger", mock.Anything)

	mockClient.On("Error").Return(nil).Times(4)
	mockClient.On("FlushAsync", mock.Anything).Return(abciclient.NewReqRes(abci.ToRequestFlush()), nil)
	mockClient.On("SetResponseCallback", mock.MatchedBy(func(cb abciclient.Callback) bool { callback = cb; return true }))

	app := kvstore.NewApplication()
	cc := proxy.NewLocalClientCreator(app)
	mp, cleanup, err := newMempoolWithAppMock(cc, mockClient)
	require.NoError(t, err)
	defer cleanup()

	// Add 4 transactions to the mempool by calling the mempool's `CheckTx` on each of them.
	txs := []types.Tx{[]byte{0x01}, []byte{0x02}, []byte{0x03}, []byte{0x04}}
	for _, tx := range txs {
		reqRes := abciclient.NewReqRes(abci.ToRequestCheckTx(abci.RequestCheckTx{Tx: tx}))
		reqRes.Response = abci.ToResponseCheckTx(abci.ResponseCheckTx{Code: abci.CodeTypeOK})

		mockClient.On("CheckTxAsync", mock.Anything, mock.Anything).Return(reqRes, nil)
		err := mp.CheckTx(tx, nil, mempool.TxInfo{})
		require.NoError(t, err)

		// ensure that the callback that the mempool sets on the ReqRes is run.
		reqRes.InvokeCallback()
	}

	// Calling update to remove the first transaction from the mempool.
	// This call also triggers the mempool to recheck its remaining transactions.
	err = mp.Update(0, []types.Tx{txs[0]}, abciResponses(1, abci.CodeTypeOK), nil, nil)
	require.Nil(t, err)

	// The mempool has now sent its requests off to the client to be rechecked
	// and is waiting for the corresponding callbacks to be called.
	// We now call the mempool-supplied callback on the first and third transaction.
	// This simulates the client dropping the second request.
	// Previous versions of this code panicked when the ABCI application missed
	// a recheck-tx request.
	resp := abci.ResponseCheckTx{Code: abci.CodeTypeOK}
	req := abci.RequestCheckTx{Tx: txs[1]}
	callback(abci.ToRequestCheckTx(req), abci.ToResponseCheckTx(resp))

	req = abci.RequestCheckTx{Tx: txs[3]}
	callback(abci.ToRequestCheckTx(req), abci.ToResponseCheckTx(resp))
	mockClient.AssertExpectations(t)
}

func TestMempool_KeepInvalidTxsInCache(t *testing.T) {
	app := kvstore.NewApplication()
	cc := proxy.NewLocalClientCreator(app)
	wcfg := config.DefaultConfig()
	wcfg.Mempool.KeepInvalidTxsInCache = true
	mp, _, cleanup := newMempoolWithAppAndConfig(cc, wcfg)
	defer cleanup()

	// 1. An invalid transaction must remain in the cache after Update
	{
		a := make([]byte, 8)
		binary.BigEndian.PutUint64(a, 0)

		b := make([]byte, 8)
		binary.BigEndian.PutUint64(b, 1)

		err := mp.CheckTx(b, nil, mempool.TxInfo{})
		require.NoError(t, err)

		// simulate new block
		_ = app.DeliverTx(abci.RequestDeliverTx{Tx: a})
		_ = app.DeliverTx(abci.RequestDeliverTx{Tx: b})
		err = mp.Update(1, []types.Tx{a, b},
			[]*abci.ResponseDeliverTx{{Code: abci.CodeTypeOK}, {Code: 2}}, nil, nil)
		require.NoError(t, err)

		// a must be added to the cache
		err = mp.CheckTx(a, nil, mempool.TxInfo{})
		if assert.Error(t, err) {
			assert.Equal(t, mempool.ErrTxInCache, err)
		}

		// b must remain in the cache
		err = mp.CheckTx(b, nil, mempool.TxInfo{})
		if assert.Error(t, err) {
			assert.Equal(t, mempool.ErrTxInCache, err)
		}
	}

	// 2. An invalid transaction must remain in the cache
	{
		a := make([]byte, 8)
		binary.BigEndian.PutUint64(a, 0)

		// remove a from the cache to test (2)
		mp.cache.Remove(a)

		err := mp.CheckTx(a, nil, mempool.TxInfo{})
		require.NoError(t, err)
	}
}

func TestTxsAvailable(t *testing.T) {
	app := kvstore.NewApplication()
	cc := proxy.NewLocalClientCreator(app)
	mp, _, cleanup := newMempoolWithApp(cc)
	defer cleanup()
	mp.EnableTxsAvailable()

	timeoutMS := 500

	// with no txs, it shouldnt fire
	ensureNoFire(t, mp.TxsAvailable(), timeoutMS)

	// send a bunch of txs, it should only fire once
	txs := checkTxs(t, mp, 100, mempool.UnknownPeerID, nil, false)
	ensureFire(t, mp.TxsAvailable(), timeoutMS)
	ensureNoFire(t, mp.TxsAvailable(), timeoutMS)

	// call update with half the txs.
	// it should fire once now for the new height
	// since there are still txs left
	committedTxs, txs := txs[:50], txs[50:]
	if err := mp.Update(1, committedTxs, abciResponses(len(committedTxs), abci.CodeTypeOK), nil, nil); err != nil {
		t.Error(err)
	}
	ensureFire(t, mp.TxsAvailable(), timeoutMS)
	ensureNoFire(t, mp.TxsAvailable(), timeoutMS)

	// send a bunch more txs. we already fired for this height so it shouldnt fire again
	moreTxs := checkTxs(t, mp, 50, mempool.UnknownPeerID, nil, false)
	ensureNoFire(t, mp.TxsAvailable(), timeoutMS)

	// now call update with all the txs. it should not fire as there are no txs left
	committedTxs = append(txs, moreTxs...)
	if err := mp.Update(2, committedTxs, abciResponses(len(committedTxs), abci.CodeTypeOK), nil, nil); err != nil {
		t.Error(err)
	}
	ensureNoFire(t, mp.TxsAvailable(), timeoutMS)

	// send a bunch more txs, it should only fire once
	checkTxs(t, mp, 100, mempool.UnknownPeerID, nil, false)
	ensureFire(t, mp.TxsAvailable(), timeoutMS)
	ensureNoFire(t, mp.TxsAvailable(), timeoutMS)
}

func TestSidecarTxsAvailable(t *testing.T) {
	app := kvstore.NewApplication()
	cc := proxy.NewLocalClientCreator(app)
	_, sidecar, cleanup := newMempoolWithApp(cc)
	defer cleanup()
	sidecar.EnableTxsAvailable()

	timeoutMS := 500

	// with no txs, it shouldnt fire
	ensureNoFire(t, sidecar.TxsAvailable(), timeoutMS)

	// send a bunch of txs, it should only fire once
	txs := addNumBundlesToSidecar(t, sidecar, 100, 10, mempool.UnknownPeerID)
	ensureFire(t, sidecar.TxsAvailable(), timeoutMS)
	ensureNoFire(t, sidecar.TxsAvailable(), timeoutMS)

	// call update with half the txs.
	// it should fire once now for the new height
	// since there are still txs left
	txs = txs[50:]

	// send a bunch more txs. we already fired for this height so it shouldnt fire again
	moreTxs := addNumBundlesToSidecar(t, sidecar, 50, 10, mempool.UnknownPeerID)
	ensureNoFire(t, sidecar.TxsAvailable(), timeoutMS)

	// now call update with all the txs. it should not fire as there are no txs left
	committedTxs := append(txs, moreTxs...) //nolint: gocritic
	if err := sidecar.Update(2, committedTxs, abciResponses(len(committedTxs), abci.CodeTypeOK)); err != nil {
		t.Error(err)
	}
	ensureNoFire(t, sidecar.TxsAvailable(), timeoutMS)

	// send a bunch more txs, it should only fire once
	addNumBundlesToSidecar(t, sidecar, 100, 10, mempool.UnknownPeerID)
	ensureFire(t, sidecar.TxsAvailable(), timeoutMS)
	ensureNoFire(t, sidecar.TxsAvailable(), timeoutMS)
}

func TestSerialReap(t *testing.T) {
	app := kvstore.NewApplication()
	cc := proxy.NewLocalClientCreator(app)

	mp, _, cleanup := newMempoolWithApp(cc)
	defer cleanup()

	appConnCon, _ := cc.NewABCIClient()
	appConnCon.SetLogger(log.TestingLogger().With("module", "abci-client", "connection", "consensus"))
	err := appConnCon.Start()
	require.Nil(t, err)

	cacheMap := make(map[string]struct{})
	deliverTxsRange := func(start, end int) {
		// Deliver some txs.
		for i := start; i < end; i++ {

			// This will succeed
			txBytes := make([]byte, 8)
			binary.BigEndian.PutUint64(txBytes, uint64(i))
			err := mp.CheckTx(txBytes, nil, mempool.TxInfo{})
			_, cached := cacheMap[string(txBytes)]
			if cached {
				require.NotNil(t, err, "expected error for cached tx")
			} else {
				require.Nil(t, err, "expected no err for uncached tx")
			}
			cacheMap[string(txBytes)] = struct{}{}

			// Duplicates are cached and should return error
			err = mp.CheckTx(txBytes, nil, mempool.TxInfo{})
			require.NotNil(t, err, "Expected error after CheckTx on duplicated tx")
		}
	}

	reapCheck := func(exp int) {
		txs := mp.ReapMaxBytesMaxGas(-1, -1, nil)
		require.Equal(t, len(txs), exp, fmt.Sprintf("Expected to reap %v txs but got %v", exp, len(txs)))
	}

	updateRange := func(start, end int) {
		txs := make([]types.Tx, 0)
		for i := start; i < end; i++ {
			txBytes := make([]byte, 8)
			binary.BigEndian.PutUint64(txBytes, uint64(i))
			txs = append(txs, txBytes)
		}
		if err := mp.Update(0, txs, abciResponses(len(txs), abci.CodeTypeOK), nil, nil); err != nil {
			t.Error(err)
		}
	}

	commitRange := func(start, end int) {
		// Deliver some txs.
		for i := start; i < end; i++ {
			txBytes := make([]byte, 8)
			binary.BigEndian.PutUint64(txBytes, uint64(i))
			res, err := appConnCon.DeliverTxSync(abci.RequestDeliverTx{Tx: txBytes})
			if err != nil {
				t.Errorf("client error committing tx: %v", err)
			}
			if res.IsErr() {
				t.Errorf("error committing tx. Code:%v result:%X log:%v",
					res.Code, res.Data, res.Log)
			}
		}
		res, err := appConnCon.CommitSync()
		if err != nil {
			t.Errorf("client error committing: %v", err)
		}
		if len(res.Data) != 8 {
			t.Errorf("error committing. Hash:%X", res.Data)
		}
	}

	//----------------------------------------

	// Deliver some txs.
	deliverTxsRange(0, 100)

	// Reap the txs.
	reapCheck(100)

	// Reap again.  We should get the same amount
	reapCheck(100)

	// Deliver 0 to 999, we should reap 900 new txs
	// because 100 were already counted.
	deliverTxsRange(0, 1000)

	// Reap the txs.
	reapCheck(1000)

	// Reap again.  We should get the same amount
	reapCheck(1000)

	// Commit from the conensus AppConn
	commitRange(0, 500)
	updateRange(0, 500)

	// We should have 500 left.
	reapCheck(500)

	// Deliver 100 invalid txs and 100 valid txs
	deliverTxsRange(900, 1100)

	// We should have 600 now.
	reapCheck(600)
}

// multiple go routines constantly try to insert bundles
// as they get reaped
func TestMempoolConcurrency(t *testing.T) {

	app := kvstore.NewApplication()
	cc := proxy.NewLocalClientCreator(app)
	_, sidecar, cleanup := newMempoolWithApp(cc)
	defer cleanup()

	var wg sync.WaitGroup

	numProcesses := 15
	numBundlesToAddPerProcess := 5
	numTxPerBundle := 10
	wg.Add(numProcesses)

	for i := 0; i < numProcesses; i++ {

		go func() {
			defer wg.Done()
			addNumBundlesToSidecar(t, sidecar, numBundlesToAddPerProcess, int64(numTxPerBundle), mempool.UnknownPeerID)
		}()

	}

	wg.Wait()

	txs := sidecar.ReapMaxTxs()
	assert.Equal(t, (numBundlesToAddPerProcess * numTxPerBundle), len(txs), "Got %d txs, expected %d",
		len(txs), (numBundlesToAddPerProcess * numTxPerBundle))
}

func TestMempool_CheckTxChecksTxSize(t *testing.T) {
	app := kvstore.NewApplication()
	cc := proxy.NewLocalClientCreator(app)

	mempl, _, cleanup := newMempoolWithApp(cc)
	defer cleanup()

	maxTxSize := mempl.config.MaxTxBytes

	testCases := []struct {
		len int
		err bool
	}{
		// check small txs. no error
		0: {10, false},
		1: {1000, false},
		2: {1000000, false},

		// check around maxTxSize
		3: {maxTxSize - 1, false},
		4: {maxTxSize, false},
		5: {maxTxSize + 1, true},
	}

	for i, testCase := range testCases {
		caseString := fmt.Sprintf("case %d, len %d", i, testCase.len)

		tx := tmrand.Bytes(testCase.len)

		err := mempl.CheckTx(tx, nil, mempool.TxInfo{})
		bv := gogotypes.BytesValue{Value: tx}
		bz, err2 := bv.Marshal()
		require.NoError(t, err2)
		require.Equal(t, len(bz), proto.Size(&bv), caseString)

		if !testCase.err {
			require.NoError(t, err, caseString)
		} else {
			require.Equal(t, err, mempool.ErrTxTooLarge{
				Max:    maxTxSize,
				Actual: testCase.len,
			}, caseString)
		}
	}
}

func TestMempoolTxsBytes(t *testing.T) {
	app := kvstore.NewApplication()
	cc := proxy.NewLocalClientCreator(app)

	cfg := config.ResetTestRoot("mempool_test")

	cfg.Mempool.MaxTxsBytes = 10
	mp, _, cleanup := newMempoolWithAppAndConfig(cc, cfg)
	defer cleanup()

	// 1. zero by default
	assert.EqualValues(t, 0, mp.SizeBytes())

	// 2. len(tx) after CheckTx
	err := mp.CheckTx([]byte{0x01}, nil, mempool.TxInfo{})
	require.NoError(t, err)
	assert.EqualValues(t, 1, mp.SizeBytes())

	// 3. zero again after tx is removed by Update
	err = mp.Update(1, []types.Tx{[]byte{0x01}}, abciResponses(1, abci.CodeTypeOK), nil, nil)
	require.NoError(t, err)
	assert.EqualValues(t, 0, mp.SizeBytes())

	// 4. zero after Flush
	err = mp.CheckTx([]byte{0x02, 0x03}, nil, mempool.TxInfo{})
	require.NoError(t, err)
	assert.EqualValues(t, 2, mp.SizeBytes())

	mp.Flush()
	assert.EqualValues(t, 0, mp.SizeBytes())

	// 5. ErrMempoolIsFull is returned when/if MaxTxsBytes limit is reached.
	err = mp.CheckTx(
		[]byte{0x04, 0x04, 0x04, 0x04, 0x04, 0x04, 0x04, 0x04, 0x04, 0x04},
		nil,
		mempool.TxInfo{},
	)
	require.NoError(t, err)

	err = mp.CheckTx([]byte{0x05}, nil, mempool.TxInfo{})
	if assert.Error(t, err) {
		assert.IsType(t, mempool.ErrMempoolIsFull{}, err)
	}

	// 6. zero after tx is rechecked and removed due to not being valid anymore
	app2 := kvstore.NewApplication()
	cc = proxy.NewLocalClientCreator(app2)

	mp, _, cleanup = newMempoolWithApp(cc)
	defer cleanup()

	txBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(txBytes, uint64(0))

	err = mp.CheckTx(txBytes, nil, mempool.TxInfo{})
	require.NoError(t, err)
	assert.EqualValues(t, 8, mp.SizeBytes())

	appConnCon, _ := cc.NewABCIClient()
	appConnCon.SetLogger(log.TestingLogger().With("module", "abci-client", "connection", "consensus"))
	err = appConnCon.Start()
	require.Nil(t, err)
	t.Cleanup(func() {
		if err := appConnCon.Stop(); err != nil {
			t.Error(err)
		}
	})

	res, err := appConnCon.DeliverTxSync(abci.RequestDeliverTx{Tx: txBytes})
	require.NoError(t, err)
	require.EqualValues(t, 0, res.Code)

	res2, err := appConnCon.CommitSync()
	require.NoError(t, err)
	require.NotEmpty(t, res2.Data)

	// Pretend like we committed nothing so txBytes gets rechecked and removed.
	err = mp.Update(1, []types.Tx{}, abciResponses(0, abci.CodeTypeOK), nil, nil)
	require.NoError(t, err)
	assert.EqualValues(t, 8, mp.SizeBytes())

	// 7. Test RemoveTxByKey function
	err = mp.CheckTx([]byte{0x06}, nil, mempool.TxInfo{})
	require.NoError(t, err)
	assert.EqualValues(t, 9, mp.SizeBytes())
	assert.Error(t, mp.RemoveTxByKey(types.Tx([]byte{0x07}).Key()))
	assert.EqualValues(t, 9, mp.SizeBytes())
	assert.NoError(t, mp.RemoveTxByKey(types.Tx([]byte{0x06}).Key()))
	assert.EqualValues(t, 8, mp.SizeBytes())

}

// This will non-deterministically catch some concurrency failures like
// https://github.com/tendermint/tendermint/issues/3509
// TODO: all of the tests should probably also run using the remote proxy app
// since otherwise we're not actually testing the concurrency of the mempool here!
func TestMempoolRemoteAppConcurrency(t *testing.T) {
	sockPath := fmt.Sprintf("unix:///tmp/echo_%v.sock", tmrand.Str(6))
	app := kvstore.NewApplication()
	_, server := newRemoteApp(t, sockPath, app)
	t.Cleanup(func() {
		if err := server.Stop(); err != nil {
			t.Error(err)
		}
	})

	cfg := config.ResetTestRoot("mempool_test")

	mp, _, cleanup := newMempoolWithAppAndConfig(proxy.NewRemoteClientCreator(sockPath, "socket", true), cfg)
	defer cleanup()

	// generate small number of txs
	nTxs := 10
	txLen := 200
	txs := make([]types.Tx, nTxs)
	for i := 0; i < nTxs; i++ {
		txs[i] = tmrand.Bytes(txLen)
	}

	// simulate a group of peers sending them over and over
	N := cfg.Mempool.Size
	maxPeers := 5
	for i := 0; i < N; i++ {
		peerID := mrand.Intn(maxPeers)
		txNum := mrand.Intn(nTxs)
		tx := txs[txNum]

		// this will err with ErrTxInCache many times ...
		mp.CheckTx(tx, nil, mempool.TxInfo{SenderID: uint16(peerID)}) //nolint: errcheck // will error
	}

	require.NoError(t, mp.FlushAppConn())
}

// caller must close server
func newRemoteApp(t *testing.T, addr string, app abci.Application) (abciclient.Client, service.Service) {
	clientCreator, err := abciclient.NewClient(addr, "socket", true)
	require.NoError(t, err)

	// Start server
	server := abciserver.NewSocketServer(addr, app)
	server.SetLogger(log.TestingLogger().With("module", "abci-server"))
	if err := server.Start(); err != nil {
		t.Fatalf("Error starting socket server: %v", err.Error())
	}

	return clientCreator, server
}

func abciResponses(n int, code uint32) []*abci.ResponseDeliverTx {
	responses := make([]*abci.ResponseDeliverTx, 0, n)
	for i := 0; i < n; i++ {
		responses = append(responses, &abci.ResponseDeliverTx{Code: code})
	}
	return responses
}
