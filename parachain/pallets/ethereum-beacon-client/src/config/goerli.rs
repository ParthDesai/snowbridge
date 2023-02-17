use crate::const_parameter_types;
use frame_support::parameter_types;

const_parameter_types! {
	pub const SlotsPerEpoch: u64 = 32;
	pub const EpochsPerSyncCommitteePeriod: u64 = 256;
	pub const MaxSyncCommitteeSize: u32 = 512;
}

#[cfg(any(test, feature = "runtime-benchmarks"))]
pub const IS_MAINNET: bool = false;
