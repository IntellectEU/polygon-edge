package polybft

import (
	"encoding/json"
	"math/big"

	"github.com/0xPolygon/polygon-edge/chain"
	"github.com/0xPolygon/polygon-edge/consensus/polybft/validator"
	"github.com/0xPolygon/polygon-edge/helper/common"
	"github.com/0xPolygon/polygon-edge/types"
)

const ConsensusName = "polybft"

// PolyBFTConfig is the configuration file for the Polybft consensus protocol.
type PolyBFTConfig struct {
	// InitialValidatorSet are the genesis validators
	InitialValidatorSet []*validator.GenesisValidator `json:"initialValidatorSet"`

	// Bridge is the rootchain bridge configuration
	Bridge *BridgeConfig `json:"bridge"`

	// EpochSize is size of epoch
	EpochSize uint64 `json:"epochSize"`

	// EpochReward is assigned to validators for blocks sealing
	EpochReward uint64 `json:"epochReward"`

	// SprintSize is size of sprint
	SprintSize uint64 `json:"sprintSize"`

	// BlockTime is target frequency of blocks production
	BlockTime common.Duration `json:"blockTime"`

	// Governance is the initial governance address
	Governance types.Address `json:"governance"`

	// NativeTokenConfig defines name, symbol and decimal count of the native token
	NativeTokenConfig *TokenConfig `json:"nativeTokenConfig"`

	InitialTrieRoot types.Hash `json:"initialTrieRoot"`

	// SupernetID indicates ID of given supernet generated by stake manager contract
	SupernetID int64 `json:"supernetID"`

	// MinValidatorSetSize indicates the minimum size of validator set
	MinValidatorSetSize uint64 `json:"minValidatorSetSize"`

	// MaxValidatorSetSize indicates the maximum size of validator set
	MaxValidatorSetSize uint64 `json:"maxValidatorSetSize"`

	// RewardConfig defines rewards configuration
	RewardConfig *RewardsConfig `json:"rewardConfig"`

	// BlockTimeDrift defines the time slot in which a new block can be created
	BlockTimeDrift uint64 `json:"blockTimeDrift"`
}

// LoadPolyBFTConfig loads chain config from provided path and unmarshals PolyBFTConfig
func LoadPolyBFTConfig(chainConfigFile string) (PolyBFTConfig, error) {
	chainCfg, err := chain.ImportFromFile(chainConfigFile)
	if err != nil {
		return PolyBFTConfig{}, err
	}

	polybftConfig, err := GetPolyBFTConfig(chainCfg)
	if err != nil {
		return PolyBFTConfig{}, err
	}

	return polybftConfig, err
}

// GetPolyBFTConfig deserializes provided chain config and returns PolyBFTConfig
func GetPolyBFTConfig(chainConfig *chain.Chain) (PolyBFTConfig, error) {
	consensusConfigJSON, err := json.Marshal(chainConfig.Params.Engine[ConsensusName])
	if err != nil {
		return PolyBFTConfig{}, err
	}

	var polyBFTConfig PolyBFTConfig
	if err = json.Unmarshal(consensusConfigJSON, &polyBFTConfig); err != nil {
		return PolyBFTConfig{}, err
	}

	return polyBFTConfig, nil
}

// BridgeConfig is the rootchain configuration, needed for bridging
type BridgeConfig struct {
	StateSenderAddr                   types.Address `json:"stateSenderAddress"`
	CheckpointManagerAddr             types.Address `json:"checkpointManagerAddress"`
	ExitHelperAddr                    types.Address `json:"exitHelperAddress"`
	RootERC20PredicateAddr            types.Address `json:"erc20PredicateAddress"`
	ChildMintableERC20PredicateAddr   types.Address `json:"erc20ChildMintablePredicateAddress"`
	RootNativeERC20Addr               types.Address `json:"nativeERC20Address"`
	ERC20TemplateAddr                 types.Address `json:"erc20TemplateAddress"`
	ERC721TemplateAddr                types.Address `json:"erc721TemplateAddress"`
	ERC1155TemplateAddr               types.Address `json:"erc1155TemplateAddress"`
	RootERC721Addr                    types.Address `json:"erc721Address"`
	RootERC721PredicateAddr           types.Address `json:"erc721PredicateAddress"`
	ChildMintableERC721PredicateAddr  types.Address `json:"erc721ChildMintablePredicateAddress"`
	RootERC1155Addr                   types.Address `json:"erc1155Address"`
	RootERC1155PredicateAddr          types.Address `json:"erc1155PredicateAddress"`
	ChildMintableERC1155PredicateAddr types.Address `json:"erc1155ChildMintablePredicateAddress"`
	CustomSupernetManagerAddr         types.Address `json:"customSupernetManagerAddr"`
	StakeManagerAddr                  types.Address `json:"stakeManagerAddr"`
	// only populated if stake-manager-deploy command is executed, and used for e2e tests
	StakeTokenAddr types.Address `json:"stakeTokenAddr,omitempty"`
	BLSAddress     types.Address `json:"blsAddr"`
	BN256G2Address types.Address `json:"bn256G2Addr"`

	JSONRPCEndpoint         string                   `json:"jsonRPCEndpoint"`
	EventTrackerStartBlocks map[types.Address]uint64 `json:"eventTrackerStartBlocks"`
}

func (p *PolyBFTConfig) IsBridgeEnabled() bool {
	return p.Bridge != nil
}

// RootchainConfig contains rootchain metadata (such as JSON RPC endpoint and contract addresses)
type RootchainConfig struct {
	JSONRPCAddr string

	StateSenderAddress                   types.Address
	CheckpointManagerAddress             types.Address
	BLSAddress                           types.Address
	BN256G2Address                       types.Address
	ExitHelperAddress                    types.Address
	RootERC20PredicateAddress            types.Address
	ChildMintableERC20PredicateAddress   types.Address
	RootNativeERC20Address               types.Address
	ERC20TemplateAddress                 types.Address
	RootERC721PredicateAddress           types.Address
	ChildMintableERC721PredicateAddress  types.Address
	RootERC721Address                    types.Address
	ERC721TemplateAddress                types.Address
	RootERC1155PredicateAddress          types.Address
	ChildMintableERC1155PredicateAddress types.Address
	RootERC1155Address                   types.Address
	ERC1155TemplateAddress               types.Address
	CustomSupernetManagerAddress         types.Address
	StakeManagerAddress                  types.Address
	StakeTokenAddress                    types.Address
}

// ToBridgeConfig creates BridgeConfig instance
func (r *RootchainConfig) ToBridgeConfig() *BridgeConfig {
	return &BridgeConfig{
		JSONRPCEndpoint: r.JSONRPCAddr,

		StateSenderAddr:                   r.StateSenderAddress,
		CheckpointManagerAddr:             r.CheckpointManagerAddress,
		ExitHelperAddr:                    r.ExitHelperAddress,
		RootERC20PredicateAddr:            r.RootERC20PredicateAddress,
		ChildMintableERC20PredicateAddr:   r.ChildMintableERC20PredicateAddress,
		RootNativeERC20Addr:               r.RootNativeERC20Address,
		ERC20TemplateAddr:                 r.ERC20TemplateAddress,
		RootERC721Addr:                    r.RootERC721Address,
		RootERC721PredicateAddr:           r.RootERC721PredicateAddress,
		ChildMintableERC721PredicateAddr:  r.ChildMintableERC721PredicateAddress,
		ERC721TemplateAddr:                r.ERC721TemplateAddress,
		RootERC1155Addr:                   r.RootERC1155Address,
		RootERC1155PredicateAddr:          r.RootERC1155PredicateAddress,
		ChildMintableERC1155PredicateAddr: r.ChildMintableERC1155PredicateAddress,
		ERC1155TemplateAddr:               r.ERC1155TemplateAddress,
		CustomSupernetManagerAddr:         r.CustomSupernetManagerAddress,
		StakeManagerAddr:                  r.StakeManagerAddress,
		BLSAddress:                        r.BLSAddress,
		BN256G2Address:                    r.BN256G2Address,
	}
}

// TokenConfig is the configuration of native token used by edge network
type TokenConfig struct {
	Name       string        `json:"name"`
	Symbol     string        `json:"symbol"`
	Decimals   uint8         `json:"decimals"`
	IsMintable bool          `json:"isMintable"`
	Owner      types.Address `json:"owner"`
}

type RewardsConfig struct {
	// TokenAddress is the address of reward token on child chain
	TokenAddress types.Address

	// WalletAddress is the address of reward wallet on child chain
	WalletAddress types.Address

	// WalletAmount is the amount of tokens in reward wallet
	WalletAmount *big.Int
}

func (r *RewardsConfig) MarshalJSON() ([]byte, error) {
	raw := &rewardsConfigRaw{
		TokenAddress:  r.TokenAddress,
		WalletAddress: r.WalletAddress,
		WalletAmount:  types.EncodeBigInt(r.WalletAmount),
	}

	return json.Marshal(raw)
}

func (r *RewardsConfig) UnmarshalJSON(data []byte) error {
	var (
		raw rewardsConfigRaw
		err error
	)

	if err = json.Unmarshal(data, &raw); err != nil {
		return err
	}

	r.TokenAddress = raw.TokenAddress
	r.WalletAddress = raw.WalletAddress

	r.WalletAmount, err = types.ParseUint256orHex(raw.WalletAmount)
	if err != nil {
		return err
	}

	return nil
}

type rewardsConfigRaw struct {
	TokenAddress  types.Address `json:"rewardTokenAddress"`
	WalletAddress types.Address `json:"rewardWalletAddress"`
	WalletAmount  *string       `json:"rewardWalletAmount"`
}
