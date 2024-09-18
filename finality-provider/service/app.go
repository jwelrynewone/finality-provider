package service

import (
	"fmt"
	"strings"
	"sync"
	"time"

	sdkmath "cosmossdk.io/math"
	bbntypes "github.com/babylonlabs-io/babylon/types"
	bstypes "github.com/babylonlabs-io/babylon/x/btcstaking/types"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/cosmos/cosmos-sdk/crypto/keyring"
	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	sdk "github.com/cosmos/cosmos-sdk/types"
	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"
	"github.com/lightningnetwork/lnd/kvdb"
	"go.uber.org/zap"

	"github.com/babylonlabs-io/finality-provider/clientcontroller"
	"github.com/babylonlabs-io/finality-provider/eotsmanager"
	"github.com/babylonlabs-io/finality-provider/eotsmanager/client"
	fpcfg "github.com/babylonlabs-io/finality-provider/finality-provider/config"
	"github.com/babylonlabs-io/finality-provider/finality-provider/proto"
	"github.com/babylonlabs-io/finality-provider/finality-provider/store"
	fpkr "github.com/babylonlabs-io/finality-provider/keyring"
	"github.com/babylonlabs-io/finality-provider/metrics"
	"github.com/babylonlabs-io/finality-provider/types"
)

type FinalityProviderApp struct {
	startOnce sync.Once
	stopOnce  sync.Once

	wg   sync.WaitGroup
	quit chan struct{}

	cc           clientcontroller.ClientController
	kr           keyring.Keyring
	fps          *store.FinalityProviderStore
	pubRandStore *store.PubRandProofStore
	config       *fpcfg.Config
	logger       *zap.Logger
	input        *strings.Reader

	fpManager   *FinalityProviderManager
	eotsManager eotsmanager.EOTSManager

	metrics *metrics.FpMetrics

	createFinalityProviderRequestChan   chan *createFinalityProviderRequest
	registerFinalityProviderRequestChan chan *registerFinalityProviderRequest
	finalityProviderRegisteredEventChan chan *finalityProviderRegisteredEvent
}

func NewFinalityProviderAppFromConfig(
	cfg *fpcfg.Config,
	db kvdb.Backend,
	logger *zap.Logger,
) (*FinalityProviderApp, error) {
	cc, err := clientcontroller.NewClientController(cfg.ChainName, cfg.BabylonConfig, &cfg.BTCNetParams, logger)
	if err != nil {
		return nil, fmt.Errorf("failed to create rpc client for the consumer chain %s: %v", cfg.ChainName, err)
	}

	// if the EOTSManagerAddress is empty, run a local EOTS manager;
	// otherwise connect a remote one with a gRPC client
	em, err := client.NewEOTSManagerGRpcClient(cfg.EOTSManagerAddress)
	if err != nil {
		return nil, fmt.Errorf("failed to create EOTS manager client: %w", err)
	}

	logger.Info("successfully connected to a remote EOTS manager", zap.String("address", cfg.EOTSManagerAddress))
	return NewFinalityProviderApp(cfg, cc, em, db, logger)
}

func NewFinalityProviderApp(
	config *fpcfg.Config,
	cc clientcontroller.ClientController,
	em eotsmanager.EOTSManager,
	db kvdb.Backend,
	logger *zap.Logger,
) (*FinalityProviderApp, error) {
	fpStore, err := store.NewFinalityProviderStore(db)
	if err != nil {
		return nil, fmt.Errorf("failed to initiate finality provider store: %w", err)
	}
	pubRandStore, err := store.NewPubRandProofStore(db)
	if err != nil {
		return nil, fmt.Errorf("failed to initiate public randomness store: %w", err)
	}

	input := strings.NewReader("")
	kr, err := fpkr.CreateKeyring(
		config.BabylonConfig.KeyDirectory,
		config.BabylonConfig.ChainID,
		config.BabylonConfig.KeyringBackend,
		input,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create keyring: %w", err)
	}

	fpMetrics := metrics.NewFpMetrics()

	fpm, err := NewFinalityProviderManager(fpStore, pubRandStore, config, cc, em, fpMetrics, logger)
	if err != nil {
		return nil, fmt.Errorf("failed to create finality-provider manager: %w", err)
	}

	return &FinalityProviderApp{
		cc:                                  cc,
		fps:                                 fpStore,
		pubRandStore:                        pubRandStore,
		kr:                                  kr,
		config:                              config,
		logger:                              logger,
		input:                               input,
		fpManager:                           fpm,
		eotsManager:                         em,
		metrics:                             fpMetrics,
		quit:                                make(chan struct{}),
		createFinalityProviderRequestChan:   make(chan *createFinalityProviderRequest),
		registerFinalityProviderRequestChan: make(chan *registerFinalityProviderRequest),
		finalityProviderRegisteredEventChan: make(chan *finalityProviderRegisteredEvent),
	}, nil
}

func (app *FinalityProviderApp) GetConfig() *fpcfg.Config {
	return app.config
}

func (app *FinalityProviderApp) GetFinalityProviderStore() *store.FinalityProviderStore {
	return app.fps
}

func (app *FinalityProviderApp) GetPubRandProofStore() *store.PubRandProofStore {
	return app.pubRandStore
}

func (app *FinalityProviderApp) GetKeyring() keyring.Keyring {
	return app.kr
}

func (app *FinalityProviderApp) GetInput() *strings.Reader {
	return app.input
}

// Logger returns the current logger of FP app.
func (app *FinalityProviderApp) Logger() *zap.Logger {
	return app.logger
}

func (app *FinalityProviderApp) ListFinalityProviderInstances() []*FinalityProviderInstance {
	return app.fpManager.ListFinalityProviderInstances()
}

func (app *FinalityProviderApp) ListAllFinalityProvidersInfo() ([]*proto.FinalityProviderInfo, error) {
	return app.fpManager.AllFinalityProviders()
}

func (app *FinalityProviderApp) GetFinalityProviderInfo(fpPk *bbntypes.BIP340PubKey) (*proto.FinalityProviderInfo, error) {
	return app.fpManager.FinalityProviderInfo(fpPk)
}

// GetFinalityProviderInstance returns the finality-provider instance with the given Babylon public key
func (app *FinalityProviderApp) GetFinalityProviderInstance(fpPk *bbntypes.BIP340PubKey) (*FinalityProviderInstance, error) {
	return app.fpManager.GetFinalityProviderInstance(fpPk)
}

func (app *FinalityProviderApp) RegisterFinalityProvider(fpPkStr string) (*RegisterFinalityProviderResponse, error) {
	fpPk, err := bbntypes.NewBIP340PubKeyFromHex(fpPkStr)
	if err != nil {
		return nil, err
	}

	fp, err := app.fps.GetFinalityProvider(fpPk.MustToBTCPK())
	if err != nil {
		return nil, err
	}

	if fp.Status != proto.FinalityProviderStatus_CREATED {
		return nil, fmt.Errorf("finality-provider is already registered")
	}

	btcSig, err := bbntypes.NewBIP340Signature(fp.Pop.BtcSig)
	if err != nil {
		return nil, err
	}

	pop := &bstypes.ProofOfPossessionBTC{
		BtcSig:     btcSig.MustMarshal(),
		BtcSigType: bstypes.BTCSigType_BIP340,
	}

	fpAddr, err := sdk.AccAddressFromBech32(fp.FPAddr)
	if err != nil {
		return nil, err
	}

	request := &registerFinalityProviderRequest{
		fpAddr:          fpAddr,
		btcPubKey:       bbntypes.NewBIP340PubKeyFromBTCPK(fp.BtcPk),
		pop:             pop,
		description:     fp.Description,
		commission:      fp.Commission,
		errResponse:     make(chan error, 1),
		successResponse: make(chan *RegisterFinalityProviderResponse, 1),
	}

	app.registerFinalityProviderRequestChan <- request

	select {
	case err := <-request.errResponse:
		return nil, err
	case successResponse := <-request.successResponse:
		return successResponse, nil
	case <-app.quit:
		return nil, fmt.Errorf("finality-provider app is shutting down")
	}
}

// StartHandlingFinalityProvider starts a finality-provider instance with the given Babylon public key
// Note: this should be called right after the finality-provider is registered
func (app *FinalityProviderApp) StartHandlingFinalityProvider(fpPk *bbntypes.BIP340PubKey, passphrase string) error {
	return app.fpManager.StartFinalityProvider(fpPk, passphrase)
}

func (app *FinalityProviderApp) StartHandlingAll() error {
	return app.fpManager.StartAll()
}

// NOTE: this is not safe in production, so only used for testing purpose
func (app *FinalityProviderApp) getFpPrivKey(fpPk []byte) (*btcec.PrivateKey, error) {
	record, err := app.eotsManager.KeyRecord(fpPk, "")
	if err != nil {
		return nil, err
	}

	return record.PrivKey, nil
}

// SyncFinalityProviderStatus syncs the status of the finality-providers with the chain.
func (app *FinalityProviderApp) SyncFinalityProviderStatus() (fpInstanceRunning bool, err error) {
	latestBlock, err := app.cc.QueryBestBlock()
	if err != nil {
		return false, err
	}

	fps, err := app.fps.GetAllStoredFinalityProviders()
	if err != nil {
		return false, err
	}

	for _, fp := range fps {
		vp, err := app.cc.QueryFinalityProviderVotingPower(fp.BtcPk, latestBlock.Height)
		if err != nil {
			// if ther error is that there is nothing in the voting power table
			// it should continue and consider the voting power
			// as zero to start the finality provider and send public randomness
			allowedErr := fmt.Sprintf("failed to query Finality Voting Power at Height %d: rpc error: code = Unknown desc = %s: unknown request", latestBlock.Height, bstypes.ErrVotingPowerTableNotUpdated.Wrapf("height: %d", latestBlock.Height).Error())
			if !strings.EqualFold(err.Error(), allowedErr) {
				// if some other error occured then the finality-provider is not registered in the Babylon chain yet
				continue
			}
		}

		if !fp.ShouldSyncStatusFromVotingPower(vp) {
			continue
		}

		bip340PubKey := fp.GetBIP340BTCPK()
		if app.fpManager.IsFinalityProviderRunning(bip340PubKey) {
			// there is a instance running, no need to keep syncing
			fpInstanceRunning = true
			// if it is already running, no need to update status
			continue
		}

		oldStatus := fp.Status
		newStatus, err := app.fps.UpdateFpStatusFromVotingPower(vp, fp)
		if err != nil {
			return false, err
		}

		app.logger.Info(
			"Update FP status",
			zap.String("fp_addr", fp.FPAddr),
			zap.String("old_status", oldStatus.String()),
			zap.String("new_status", newStatus.String()),
		)
		fp.Status = newStatus

		if !fp.ShouldStart() {
			continue
		}

		if err := app.fpManager.StartFinalityProvider(bip340PubKey, ""); err != nil {
			return false, err
		}
		fpInstanceRunning = true
	}

	return fpInstanceRunning, nil
}

// Start starts only the finality-provider daemon without any finality-provider instances
func (app *FinalityProviderApp) Start() error {
	var startErr error
	app.startOnce.Do(func() {
		app.logger.Info("Starting FinalityProviderApp")

		app.wg.Add(4)
		go app.syncChainFpStatusLoop()
		go app.eventLoop()
		go app.registrationLoop()
		go app.metricsUpdateLoop()
	})

	return startErr
}

func (app *FinalityProviderApp) Stop() error {
	var stopErr error
	app.stopOnce.Do(func() {
		app.logger.Info("Stopping FinalityProviderApp")

		// Always stop the submission loop first to not generate additional events and actions
		app.logger.Debug("Stopping submission loop")
		close(app.quit)
		app.wg.Wait()

		app.logger.Debug("Stopping finality providers")
		if err := app.fpManager.Stop(); err != nil {
			stopErr = err
			return
		}

		app.logger.Debug("Stopping EOTS manager")
		if err := app.eotsManager.Close(); err != nil {
			stopErr = err
			return
		}

		app.logger.Debug("FinalityProviderApp successfully stopped")

	})
	return stopErr
}

func (app *FinalityProviderApp) CreateFinalityProvider(
	keyName, chainID, passPhrase, hdPath string,
	eotsPk *bbntypes.BIP340PubKey,
	description *stakingtypes.Description,
	commission *sdkmath.LegacyDec,
) (*CreateFinalityProviderResult, error) {

	req := &createFinalityProviderRequest{
		keyName:         keyName,
		chainID:         chainID,
		passPhrase:      passPhrase,
		hdPath:          hdPath,
		eotsPk:          eotsPk,
		description:     description,
		commission:      commission,
		errResponse:     make(chan error, 1),
		successResponse: make(chan *createFinalityProviderResponse, 1),
	}

	app.createFinalityProviderRequestChan <- req

	select {
	case err := <-req.errResponse:
		return nil, err
	case successResponse := <-req.successResponse:
		return &CreateFinalityProviderResult{
			FpInfo: successResponse.FpInfo,
		}, nil
	case <-app.quit:
		return nil, fmt.Errorf("finality-provider app is shutting down")
	}
}

func (app *FinalityProviderApp) handleCreateFinalityProviderRequest(req *createFinalityProviderRequest) (*createFinalityProviderResponse, error) {
	// 1. check if the chain key exists
	kr, err := fpkr.NewChainKeyringControllerWithKeyring(app.kr, req.keyName, app.input)
	if err != nil {
		return nil, err
	}

	fpAddr, err := kr.Address(req.passPhrase)
	if err != nil {
		// the chain key does not exist, should create the chain key first
		keyInfo, err := kr.CreateChainKey(req.passPhrase, req.hdPath, "")
		if err != nil {
			return nil, fmt.Errorf("failed to create chain key %s: %w", req.keyName, err)
		}
		fpAddr = keyInfo.AccAddress
	}

	// 2. create EOTS key
	fpPk := req.eotsPk
	if req.eotsPk == nil {
		fpPkBytes, err := app.eotsManager.CreateKey(req.keyName, req.passPhrase, req.hdPath)
		if err != nil {
			return nil, err
		}
		fpPk, err = bbntypes.NewBIP340PubKey(fpPkBytes)
		if err != nil {
			return nil, err
		}
	}

	fpRecord, err := app.eotsManager.KeyRecord(fpPk.MustMarshal(), req.passPhrase)
	if err != nil {
		return nil, fmt.Errorf("failed to get finality-provider record: %w", err)
	}

	// 3. create proof-of-possession
	pop, err := kr.CreatePop(fpAddr, fpRecord.PrivKey)
	if err != nil {
		return nil, fmt.Errorf("failed to create proof-of-possession of the finality-provider: %w", err)
	}

	if err := app.fps.CreateFinalityProvider(fpAddr, fpPk.MustToBTCPK(), req.description, req.commission, req.keyName, req.chainID, pop.BtcSig); err != nil {
		return nil, fmt.Errorf("failed to save finality-provider: %w", err)
	}
	app.fpManager.metrics.RecordFpStatus(fpPk.MarshalHex(), proto.FinalityProviderStatus_CREATED)

	app.logger.Info("successfully created a finality-provider",
		zap.String("btc_pk", fpPk.MarshalHex()),
		zap.String("addr", fpAddr.String()),
		zap.String("key_name", req.keyName),
	)

	storedFp, err := app.fps.GetFinalityProvider(fpPk.MustToBTCPK())
	if err != nil {
		return nil, err
	}

	return &createFinalityProviderResponse{
		FpInfo: storedFp.ToFinalityProviderInfo(),
	}, nil
}

// SignRawMsg loads the keyring private key and signs a message.
func (app *FinalityProviderApp) SignRawMsg(
	keyName, passPhrase, hdPath string,
	rawMsgToSign []byte,
) ([]byte, error) {
	_, chainSk, err := app.loadChainKeyring(keyName, passPhrase, hdPath)
	if err != nil {
		return nil, err
	}

	return chainSk.Sign(rawMsgToSign)
}

// loadChainKeyring checks the keyring by loading or creating a chain key.
func (app *FinalityProviderApp) loadChainKeyring(
	keyName, passPhrase, hdPath string,
) (*fpkr.ChainKeyringController, *secp256k1.PrivKey, error) {
	kr, err := fpkr.NewChainKeyringControllerWithKeyring(app.kr, keyName, app.input)
	if err != nil {
		return nil, nil, err
	}
	chainSk, err := kr.GetChainPrivKey(passPhrase)
	if err != nil {
		// the chain key does not exist, should create the chain key first
		keyInfo, err := kr.CreateChainKey(passPhrase, hdPath, "")
		if err != nil {
			return nil, nil, fmt.Errorf("failed to create chain key %s: %w", keyName, err)
		}
		chainSk = &secp256k1.PrivKey{Key: keyInfo.PrivateKey.Serialize()}
	}

	return kr, chainSk, nil
}

// UpdateClientController sets a new client controoller in the App.
// Usefull for testing with multiples PKs with different keys, it needs
// to update who is the signer
func (app *FinalityProviderApp) UpdateClientController(cc clientcontroller.ClientController) {
	app.cc = cc
}

// StoreFinalityProvider stores a new finality provider in the fp store.
func (app *FinalityProviderApp) StoreFinalityProvider(
	keyName, passPhrase, hdPath, chainID string,
	description *stakingtypes.Description,
	commission *sdkmath.LegacyDec,
) (*store.StoredFinalityProvider, error) {
	// 1. check if the chain key exists
	kr, _, err := app.loadChainKeyring(keyName, passPhrase, hdPath)
	if err != nil {
		return nil, err
	}
	fpAddr, err := kr.Address(passPhrase)
	if err != nil {
		return nil, err
	}

	// 2. create EOTS key
	fpPkBytes, err := app.eotsManager.CreateKey(keyName, passPhrase, hdPath)
	if err != nil {
		return nil, err
	}
	fpPk, err := bbntypes.NewBIP340PubKey(fpPkBytes)
	if err != nil {
		return nil, err
	}
	fpRecord, err := app.eotsManager.KeyRecord(fpPk.MustMarshal(), passPhrase)
	if err != nil {
		return nil, fmt.Errorf("failed to get finality-provider record: %w", err)
	}

	// 3. create proof-of-possession
	pop, err := kr.CreatePop(fpAddr, fpRecord.PrivKey)
	if err != nil {
		return nil, fmt.Errorf("failed to create proof-of-possession of the finality provider: %w", err)
	}

	if err := app.fps.CreateFinalityProvider(fpAddr, fpPk.MustToBTCPK(), description, commission, keyName, chainID, pop.BtcSig); err != nil {
		return nil, fmt.Errorf("failed to save finality-provider: %w", err)
	}
	app.fpManager.metrics.RecordFpStatus(fpPk.MarshalHex(), proto.FinalityProviderStatus_CREATED)

	app.logger.Info("successfully created a finality-provider",
		zap.String("btc_pk", fpPk.MarshalHex()),
		zap.String("fp_addr", fpAddr.String()),
		zap.String("key_name", keyName),
	)

	storedFp, err := app.fps.GetFinalityProvider(fpPk.MustToBTCPK())
	if err != nil {
		return nil, err
	}

	return storedFp, nil
}

func CreateChainKey(keyringDir, chainID, keyName, backend, passphrase, hdPath, mnemonic string) (*types.ChainKeyInfo, error) {
	sdkCtx, err := fpkr.CreateClientCtx(
		keyringDir, chainID,
	)
	if err != nil {
		return nil, err
	}

	krController, err := fpkr.NewChainKeyringController(
		sdkCtx,
		keyName,
		backend,
	)
	if err != nil {
		return nil, err
	}

	return krController.CreateChainKey(passphrase, hdPath, mnemonic)
}

// main event loop for the finality-provider app
func (app *FinalityProviderApp) eventLoop() {
	defer app.wg.Done()

	for {
		select {
		case req := <-app.createFinalityProviderRequestChan:
			res, err := app.handleCreateFinalityProviderRequest(req)
			if err != nil {
				req.errResponse <- err
				continue
			}

			req.successResponse <- &createFinalityProviderResponse{FpInfo: res.FpInfo}

		case ev := <-app.finalityProviderRegisteredEventChan:
			// change the status of the finality-provider to registered
			err := app.fps.SetFpStatus(ev.btcPubKey.MustToBTCPK(), proto.FinalityProviderStatus_REGISTERED)
			if err != nil {
				app.logger.Fatal("failed to set finality-provider status to REGISTERED",
					zap.String("pk", ev.btcPubKey.MarshalHex()),
					zap.Error(err),
				)
			}
			app.fpManager.metrics.RecordFpStatus(ev.btcPubKey.MarshalHex(), proto.FinalityProviderStatus_REGISTERED)

			// return to the caller
			ev.successResponse <- &RegisterFinalityProviderResponse{
				bbnAddress: ev.bbnAddress,
				btcPubKey:  ev.btcPubKey,
				TxHash:     ev.txHash,
			}

		case <-app.quit:
			app.logger.Debug("exiting main event loop")
			return
		}
	}
}

func (app *FinalityProviderApp) registrationLoop() {
	defer app.wg.Done()
	for {
		select {
		case req := <-app.registerFinalityProviderRequestChan:
			// we won't do any retries here to not block the loop for more important messages.
			// Most probably it fails due so some user error so we just return the error to the user.
			// TODO: need to start passing context here to be able to cancel the request in case of app quiting
			popBytes, err := req.pop.Marshal()
			if err != nil {
				req.errResponse <- err
				continue
			}

			desBytes, err := req.description.Marshal()
			if err != nil {
				req.errResponse <- err
				continue
			}
			res, err := app.cc.RegisterFinalityProvider(
				req.btcPubKey.MustToBTCPK(),
				popBytes,
				req.commission,
				desBytes,
			)

			if err != nil {
				app.logger.Error(
					"failed to register finality-provider",
					zap.String("pk", req.btcPubKey.MarshalHex()),
					zap.Error(err),
				)
				req.errResponse <- err
				continue
			}

			app.logger.Info(
				"successfully registered finality-provider on babylon",
				zap.String("btc_pk", req.btcPubKey.MarshalHex()),
				zap.String("fp_addr", req.fpAddr.String()),
				zap.String("txHash", res.TxHash),
			)

			app.finalityProviderRegisteredEventChan <- &finalityProviderRegisteredEvent{
				btcPubKey:  req.btcPubKey,
				bbnAddress: req.fpAddr,
				txHash:     res.TxHash,
				// pass the channel to the event so that we can send the response to the user which requested
				// the registration
				successResponse: req.successResponse,
			}
		case <-app.quit:
			app.logger.Debug("exiting registration loop")
			return
		}
	}
}

func (app *FinalityProviderApp) metricsUpdateLoop() {
	defer app.wg.Done()

	interval := app.config.Metrics.UpdateInterval
	app.logger.Info("starting metrics update loop",
		zap.Float64("interval seconds", interval.Seconds()))
	updateTicker := time.NewTicker(interval)

	for {
		select {
		case <-updateTicker.C:
			fps, err := app.fps.GetAllStoredFinalityProviders()
			if err != nil {
				app.logger.Error("failed to get finality-providers from the store", zap.Error(err))
				continue
			}
			app.metrics.UpdateFpMetrics(fps)
		case <-app.quit:
			updateTicker.Stop()
			app.logger.Info("exiting metrics update loop")
			return
		}
	}
}

// syncChainFpStatusLoop keeps querying the chain for the finality
// provider voting power and update the FP status accordingly.
// If there is some voting power it sets to active, for zero voting power
// it goes from: CREATED -> REGISTERED or ACTIVE -> INACTIVE.
// if there is any node running or a new finality provider instance
// is started, the loop stops.
func (app *FinalityProviderApp) syncChainFpStatusLoop() {
	defer app.wg.Done()

	interval := app.config.SyncFpStatusInterval
	app.logger.Info(
		"starting sync FP status loop",
		zap.Float64("interval seconds", interval.Seconds()),
	)
	syncFpStatusTicker := time.NewTicker(interval)

	for {
		select {
		case <-syncFpStatusTicker.C:
			fpInstanceStarted, err := app.SyncFinalityProviderStatus()
			if err != nil {
				app.Logger().Error("failed to sync finality-provider status", zap.Error(err))
			}
			if fpInstanceStarted {
				return
			}

		case <-app.quit:
			syncFpStatusTicker.Stop()
			app.logger.Info("exiting sync FP status loop")
			return
		}
	}
}