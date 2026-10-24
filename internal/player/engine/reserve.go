package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"time"

	"github.com/google/uuid"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
)

// idBlock is how many IDs, and how many tokens, one reservation covers.
const idBlock = 4096

const (
	// reserveVersion is the reservation file's format.
	reserveVersion = 2
	// maxReserveFileBytes caps the reservation file read at a start.
	maxReserveFileBytes = 4 << 10
	// reserveTimeout bounds how long the edits that need a reservation wait
	// for its write to land: past it they are refused (ReasonReservation),
	// and so is every edit that needs one, until the write lands.
	reserveTimeout = 5 * time.Second
)

// reserveFile is the reservation file: the highest ID and the highest
// token any run may hand out without writing the file again. It is written,
// and it and its directory entry fsynced, before any ID or token it covers
// is handed out.
type reserveFile = Reservation

// ReservationFileName is serve's reservation beside player-uuid. Backups
// carry it as an input to a restore, never as a file to copy back.
const ReservationFileName = "player-ids.json"

// Reservation holds the highest ID and token reserved by UUID. Version 1
// has no UUID and remains a floor; every writer writes version 2.
type Reservation struct {
	Version int    `json:"version"`
	UUID    string `json:"uuid,omitempty"`
	IDs     uint32 `json:"ids"`
	Tokens  uint32 `json:"tokens"`
}

// Exhausted reports whether either counter cannot start another run, using
// the same skipped block as the engine's startCounter.
func (r Reservation) Exhausted() bool {
	ids, tokens := startCounter(uint64(r.IDs)), startCounter(uint64(r.Tokens))
	return ids.exhausted() || tokens.exhausted()
}

// encodeReserve is the file for the given reservations.
func encodeReserve(id string, ids, tokens uint64) []byte {
	data, err := json.Marshal(reserveFile{Version: reserveVersion, UUID: id, IDs: uint32(ids), Tokens: uint32(tokens)})
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
	if f.Version != 1 && f.Version != reserveVersion {
		return f, fmt.Errorf("version %d, not %d", f.Version, reserveVersion)
	}
	if f.Version == 2 && !validPlayerUUID(f.UUID) || f.Version == 1 && f.UUID != "" {
		return f, errors.New("reservation does not name one lowercase UUID for its version")
	}
	return f, nil
}

// validPlayerUUID accepts the same canonical lowercase form as player-uuid.
func validPlayerUUID(id string) bool {
	u, err := uuid.Parse(id)
	return err == nil && u.String() == id
}

// ReadReservation reads one bounded reservation. A missing file is an error
// wrapping os.ErrNotExist; callers decide whether absence means no floor.
func ReadReservation(path string) (Reservation, error) {
	b, err := readLimited(path, maxReserveFileBytes)
	if err != nil {
		return Reservation{}, err
	}
	return decodeReserve(b)
}

// WriteReservation merges each mark with the live file, whatever UUID it
// names, then durably writes version 2 with the writer's UUID. The caller
// must hold the data directory's exclusive lock and serialize its writes.
// An unreadable live file refuses the write; it is never silently lowered.
func WriteReservation(dataDir, path string, want Reservation) error {
	return mergeReservation(path, want, readLimited, func(path string, b []byte) error {
		return atomicwrite.WriteBytesDurable(dataDir, path, b, ".player-ids-*.tmp")
	})
}

// mergeReservation is shared by restore and the engine's injectable writer.
func mergeReservation(path string, want Reservation, read func(string, int64) ([]byte, error), write func(string, []byte) error) error {
	if !validPlayerUUID(want.UUID) {
		return errors.New("reservation writer requires one lowercase player UUID")
	}
	b, err := read(path, maxReserveFileBytes)
	if err == nil {
		old, err := decodeReserve(b)
		if err != nil {
			return err
		}
		want.IDs = max(want.IDs, old.IDs)
		want.Tokens = max(want.Tokens, old.Tokens)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return write(path, encodeReserve(want.UUID, uint64(want.IDs), uint64(want.Tokens)))
}

// writeReservation uses the engine's UUID for its first and every later write.
func (e *Engine) writeReservation(ids, tokens uint64) error {
	return mergeReservation(e.cfg.ReservePath, Reservation{UUID: e.cfg.PlayerUUID, IDs: uint32(ids), Tokens: uint32(tokens)}, e.sm.readFile, e.sm.writeReserve)
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
	// bound ends the wait at reserveTimeout (msgReserveOverdue, numbered
	// by waits); overdue is a write past it, which refuses every edit that
	// needs a value until it lands.
	bound   timer
	waits   uint64
	overdue bool
	// failed is the last write's failure until one lands; refuseParked
	// makes the commands that waited on a failed write refuse rather than
	// write again.
	failed       error
	refuseParked bool
	// stuckQueue is a corrupt queue that could not be moved aside. Until
	// it is removed, token-advancing edits and queue writes are refused.
	// An unreadable reservation instead refuses New; it is never discarded.
	stuckQueue string
	// exhaustedLogged is the one log line for exhausted IDs or tokens.
	exhaustedLogged bool
}
