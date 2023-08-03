package txpool_instance

import "time"

var (
	txSlotSize uint64 = 32 * 1024

	txMaxSize = 4 * txSlotSize // 128KB

	evictionInterval    = time.Minute     // Time interval to check for evictable transactions
	statsReportInterval = 8 * time.Second // Time interval to report transaction pool stats
)
