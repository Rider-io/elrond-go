package config

// EpochConfig will hold epoch configurations
type EpochConfig struct {
	EnableEpochs EnableEpochs
	GasSchedule  GasScheduleConfig
}

// GasScheduleConfig represents the versioning config area for the gas schedule toml
type GasScheduleConfig struct {
	GasScheduleByEpochs []GasScheduleByEpochs
}

// EnableEpochs will hold the configuration for activation epochs
type EnableEpochs struct {
	SCDeployEnableEpoch                         uint32
	BuiltInFunctionsEnableEpoch                 uint32
	RelayedTransactionsEnableEpoch              uint32
	PenalizedTooMuchGasEnableEpoch              uint32
	SwitchJailWaitingEnableEpoch                uint32
	SwitchHysteresisForMinNodesEnableEpoch      uint32
	BelowSignedThresholdEnableEpoch             uint32
	TransactionSignedWithTxHashEnableEpoch      uint32
	MetaProtectionEnableEpoch                   uint32
	AheadOfTimeGasUsageEnableEpoch              uint32
	GasPriceModifierEnableEpoch                 uint32
	RepairCallbackEnableEpoch                   uint32
	MaxNodesChangeEnableEpoch                   []MaxNodesChangeConfig
	BlockGasAndFeesReCheckEnableEpoch           uint32
	StakingV2EnableEpoch                        uint32
	StakeEnableEpoch                            uint32
	DoubleKeyProtectionEnableEpoch              uint32
	ESDTEnableEpoch                             uint32
	GovernanceEnableEpoch                       uint32
	DelegationManagerEnableEpoch                uint32
	DelegationSmartContractEnableEpoch          uint32
	CorrectLastUnjailedEnableEpoch              uint32
	BalanceWaitingListsEnableEpoch              uint32
	ReturnDataToLastTransferEnableEpoch         uint32
	SenderInOutTransferEnableEpoch              uint32
	RelayedTransactionsV2EnableEpoch            uint32
	UnbondTokensV2EnableEpoch                   uint32
	SaveJailedAlwaysEnableEpoch                 uint32
	ValidatorToDelegationEnableEpoch            uint32
	ReDelegateBelowMinCheckEnableEpoch          uint32
	WaitingListFixEnableEpoch                   uint32
	IncrementSCRNonceInMultiTransferEnableEpoch uint32
	ESDTMultiTransferEnableEpoch                uint32
	GlobalMintBurnDisableEpoch                  uint32
	ESDTTransferRoleEnableEpoch                 uint32
	BuiltInFunctionOnMetaEnableEpoch            uint32
}

// GasScheduleByEpochs represents a gas schedule toml entry that will be applied from the provided epoch
type GasScheduleByEpochs struct {
	StartEpoch uint32
	FileName   string
}
