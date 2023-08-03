package params

var (
	// TxGas                     uint64 = 21000 // Per transaction not creating a contract. NOTE: Not payable on data of calls between transactions.
	// TxGasContractCreation     uint64 = 53000 // Per transaction that creates a contract. NOTE: Not payable on data of calls between transactions.
	// TxDataZeroGas             uint64 = 4     // Per byte of data attached to a transaction that equals zero. NOTE: Not payable on data of calls between transactions.
	TxDataNonZeroGas uint64 = 16 // Per byte of non zero data attached to a transaction after EIP 2028 (part in Istanbul)
	// TxAccessListAddressGas    uint64 = 2400  // Per address specified in EIP 2930 access list
	// TxAccessListStorageKeyGas uint64 = 1900  // Per storage key specified in EIP 2930 access list
	// InitCodeWordGas           uint64 = 2     // Per word of initialisation code for a contract
)
