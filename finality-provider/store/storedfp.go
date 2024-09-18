package store

import (
	"fmt"

	sdkmath "cosmossdk.io/math"
	bbn "github.com/babylonlabs-io/babylon/types"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"

	"github.com/babylonlabs-io/finality-provider/finality-provider/proto"
)

type StoredFinalityProvider struct {
	FPAddr              string
	BtcPk               *btcec.PublicKey
	Description         *stakingtypes.Description
	Commission          *sdkmath.LegacyDec
	Pop                 *proto.ProofOfPossession
	KeyName             string
	ChainID             string
	LastVotedHeight     uint64
	LastProcessedHeight uint64
	Status              proto.FinalityProviderStatus
}

func protoFpToStoredFinalityProvider(fp *proto.FinalityProvider) (*StoredFinalityProvider, error) {
	btcPk, err := schnorr.ParsePubKey(fp.BtcPk)
	if err != nil {
		return nil, fmt.Errorf("invalid BTC public key: %w", err)
	}

	var des stakingtypes.Description
	if err := des.Unmarshal(fp.Description); err != nil {
		return nil, fmt.Errorf("invalid description: %w", err)
	}

	commission, err := sdkmath.LegacyNewDecFromStr(fp.Commission)
	if err != nil {
		return nil, fmt.Errorf("invalid commission: %w", err)
	}

	return &StoredFinalityProvider{
		FPAddr:      fp.FpAddr,
		BtcPk:       btcPk,
		Description: &des,
		Commission:  &commission,
		Pop: &proto.ProofOfPossession{
			BtcSig: fp.Pop.BtcSig,
		},
		KeyName:             fp.KeyName,
		ChainID:             fp.ChainId,
		LastVotedHeight:     fp.LastVotedHeight,
		LastProcessedHeight: fp.LastProcessedHeight,
		Status:              fp.Status,
	}, nil
}

func (sfp *StoredFinalityProvider) GetBIP340BTCPK() *bbn.BIP340PubKey {
	return bbn.NewBIP340PubKeyFromBTCPK(sfp.BtcPk)
}

func (sfp *StoredFinalityProvider) ToFinalityProviderInfo() *proto.FinalityProviderInfo {
	return &proto.FinalityProviderInfo{
		FpAddr:   sfp.FPAddr,
		BtcPkHex: sfp.GetBIP340BTCPK().MarshalHex(),
		Description: &proto.Description{
			Moniker:         sfp.Description.Moniker,
			Identity:        sfp.Description.Identity,
			Website:         sfp.Description.Website,
			SecurityContact: sfp.Description.SecurityContact,
			Details:         sfp.Description.Details,
		},
		Commission:      sfp.Commission.String(),
		LastVotedHeight: sfp.LastVotedHeight,
		Status:          sfp.Status.String(),
	}
}

// ShouldSyncStatusFromVotingPower returns true if the status should be updated
// based on the provided voting power or the current status of the finality provider.
//
// It returns true if the voting power is greater than zero, or if the status
// is either 'CREATED' or 'ACTIVE'.
func (sfp *StoredFinalityProvider) ShouldSyncStatusFromVotingPower(vp uint64) bool {
	if vp > 0 {
		return true
	}

	return sfp.Status == proto.FinalityProviderStatus_CREATED ||
		sfp.Status == proto.FinalityProviderStatus_ACTIVE
}

// ShouldStart returns true if the finality provider should start his instance
// based on the current status of the finality provider.
//
// It returns false if the status is either 'CREATED' or 'SLASHED'.
// It returs true for all the other status.
func (sfp *StoredFinalityProvider) ShouldStart() bool {
	if sfp.Status == proto.FinalityProviderStatus_CREATED || sfp.Status == proto.FinalityProviderStatus_SLASHED {
		return false
	}

	return true
}