package engine

import (
	"encoding/json"
	"fmt"
	"math"
)

// idBlock is how many IDs, and how many tokens, one reservation covers.
const idBlock = 4096

const (
	// reserveVersion is the reservation file's format.
	reserveVersion = 1
	// maxReserveFileBytes caps the reservation file read at a start.
	maxReserveFileBytes = 4 << 10
)

// reserveFile is the reservation file: the highest ID and the highest
// token any run may hand out without writing the file again. It is written,
// and it and its directory entry fsynced, before any ID or token it covers
// is handed out.
type reserveFile struct {
	Version int    `json:"version"`
	IDs     uint32 `json:"ids"`
	Tokens  uint32 `json:"tokens"`
}

// encodeReserve is the file for the given reservations.
func encodeReserve(ids, tokens uint64) []byte {
	data, err := json.Marshal(reserveFile{Version: reserveVersion, IDs: uint32(ids), Tokens: uint32(tokens)})
	if err != nil {
		panic(err) // three integers always encode
	}
	return data
}

// decodeReserve reads a reservation file, refusing anything it would not
// write.
func decodeReserve(data []byte) (reserveFile, error) {
	var f reserveFile
	if len(data) > maxReserveFileBytes {
		return f, fmt.Errorf("%d bytes, more than %d", len(data), maxReserveFileBytes)
	}
	if err := strictJSON(data, &f); err != nil {
		return f, err
	}
	if f.Version != reserveVersion {
		return f, fmt.Errorf("version %d, not %d", f.Version, reserveVersion)
	}
	return f, nil
}

// counter hands out IDs, or tokens: never 0 and never a value any run has
// handed out, each inside a reservation already durable. Values are kept
// in a uint64 so the end of uint32 is a value it can name.
type counter struct {
	next    uint64 // the next value to hand out
	durable uint64 // the highest value a durable reservation covers
	want    uint64 // the highest value the reservation being written covers
}

// startCounter starts past base by one block, a margin for one reservation
// that storage acknowledged and did not keep: one block past what
// survived. A reservation whose directory fsync failed was never made
// (atomicwrite.WriteBytesDurable), so nothing was handed out from it.
func startCounter(base uint64) counter {
	return counter{next: base + idBlock + 1, durable: base}
}

// exhausted reports a counter past the last value a uint32 holds.
func (c *counter) exhausted() bool { return c.next > math.MaxUint32 }

// needs reports a counter whose next value no durable reservation covers.
func (c *counter) needs() bool { return c.next > c.durable }

// low reports a counter with less than a block reserved ahead of it.
func (c *counter) low() bool { return c.durable < c.next-1+idBlock && c.durable < math.MaxUint32 }

// target is the reservation that covers a block from the next value, or
// what is durable when that is more.
func (c *counter) target() uint64 {
	return min(max(c.durable, c.next-1)+idBlock, math.MaxUint32)
}

// take hands out the next value; the caller asked exhausted and needs.
func (c *counter) take() uint32 {
	v := c.next
	c.next++
	return uint32(v)
}

// reservation is the owner's account of the reservation file.
type reservation struct {
	ids, tokens counter
	// inflight is a write under way; awaiting, that commands wait on it.
	inflight, awaiting bool
	// failed is the last write's failure until one lands; refuseParked
	// makes the commands that waited on a failed write refuse rather than
	// write again.
	failed       error
	refuseParked bool
	// stuckQueue and stuckReserve are a bad queue file and a bad
	// reservation file that could not be moved aside. While either stays,
	// every insert and every other token-advancing edit is refused; while
	// the queue file stays no queue file is written, and while the
	// reservation file stays no reservation is, so neither is overwritten.
	stuckQueue, stuckReserve string
	// exhaustedLogged is the one log line for exhausted IDs or tokens.
	exhaustedLogged bool
}
