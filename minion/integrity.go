package main

import "github.com/logoscore/logos-golang-protocol/protocol"

// integrityOfRID is a Windows mandatory label's level by its RID: below
// medium (0x2000) low, below high (0x3000) medium, below system (0x4000)
// high, and system from there (a protected process's too).
func integrityOfRID(rid uint32) int {
	switch {
	case rid < 0x2000:
		return protocol.IntegrityLow
	case rid < 0x3000:
		return protocol.IntegrityMedium
	case rid < 0x4000:
		return protocol.IntegrityHigh
	}
	return protocol.IntegritySystem
}
