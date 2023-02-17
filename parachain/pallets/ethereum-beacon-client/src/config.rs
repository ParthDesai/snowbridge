use frame_support::parameter_types;

#[cfg(feature = "mainnet")]
mod mainnet;
#[cfg(feature = "mainnet")]
pub use mainnet::*;

#[cfg(not(feature = "mainnet"))]
mod goerli;

#[cfg(not(feature = "mainnet"))]
pub use goerli::*;

parameter_types! {
	pub const CurrentSyncCommitteeIndex: u64 = 22;
	pub const CurrentSyncCommitteeDepth: u64 = 5;
	pub const MaxProofBranchSize: u32 = 6;

	pub const NextSyncCommitteeDepth: u64 = 5;
	pub const NextSyncCommitteeIndex: u64 = 23;

	pub const FinalizedRootDepth: u64 = 6;
	pub const FinalizedRootIndex: u64 = 41;

	pub const MaxProposerSlashingSize: u32 = 16;
	pub const MaxAttesterSlashingSize: u32 = 2;
	pub const MaxAttestationSize: u32 = 128;
	pub const MaxDepositDataSize: u32 = 16;
	pub const MaxVoluntaryExitSize: u32 = 16;
	pub const MaxValidatorsPerCommittee: u32 = 2048;
	pub const MaxExtraDataSize: u32 = 32;
	pub const MaxLogsBloomSize: u32 = 256;
	pub const MaxFeeRecipientSize: u32 = 20;

	pub const DepositContractTreeDepth: usize = 32;

	/// DomainType('0x07000000')
	/// https://github.com/ethereum/consensus-specs/blob/dev/specs/altair/beacon-chain.md#domain-types
	pub const DomainSyncCommittee: [u8; 4] = [7, 0, 0, 0];

	pub const MaxPublicKeySize: u32 = 48;
	pub const MaxSignatureSize: u32 = 96;

	pub const GenesisSlot: u64 = 0;
}
