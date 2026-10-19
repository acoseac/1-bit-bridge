package alsa

import (
	"regexp"
	"strings"
)

// Card is one entry of /proc/asound/cards.
type Card struct {
	// Index is the card number, the N of /proc/asound/cardN.
	Index int `json:"index"`
	// ID is the card's id, the name ALSA addresses it by ("Audio" in
	// hw:Audio) and the symlink /proc/asound/<id> points at cardN.
	ID string `json:"id"`
	// Driver is the kernel driver's name for the card ("USB-Audio").
	Driver string `json:"driver"`
	// Name is the card's short name, what `aplay -l` prints in brackets.
	Name string `json:"name"`
	// LongName is the second line of the entry; for a USB card it names
	// the bus and the speed the device enumerated at.
	LongName string `json:"longName"`
}

// cardHead matches an entry's first line, which the kernel prints as
// "%2i [%-15s]: %s - %s" (index, id, driver, short name).
var cardHead = regexp.MustCompile(`^ ?(\d+) \[([^\]]*)\]: (.*)$`)

// noSoundcards is what the kernel prints when no card is registered.
const noSoundcards = "--- no soundcards ---"

// ParseCards parses /proc/asound/cards. Each card is two lines, the head
// and its long name indented by 22 spaces; a line that begins with two
// spaces is therefore the long name of the entry before it, never a head,
// since the head's index is printed "%2i" and has at most one.
//
// A card whose index repeats an earlier one is reported and left out, so
// the reader never reads one card directory twice. A head with no
// long-name line after it is kept and reported: the kernel prints the two
// together, so the file was cut short there.
func ParseCards(b []byte) ([]Card, []string) {
	var p problems
	cards := []Card{}
	seen := map[int]bool{}
	// pending is the card whose long-name line is due next, and pendingLine
	// the line its head is on; a head sets them and the next line clears
	// them.
	pending, pendingLine := -1, 0
	unfinished := func() {
		if pending >= 0 {
			p.addf(pendingLine, "card %d has no long-name line", cards[pending].Index)
		}
		pending = -1
	}
	for i, line := range lines(b) {
		ln := i + 1
		if strings.HasPrefix(line, "  ") {
			if pending >= 0 {
				cards[pending].LongName = strings.TrimSpace(line)
				pending = -1
			} else if strings.TrimSpace(line) != "" {
				p.addf(ln, "a long-name line with no card before it: %s", quote(line))
			}
			continue
		}
		unfinished()
		if t := strings.TrimSpace(line); t == "" || t == noSoundcards {
			continue
		}
		m := cardHead.FindStringSubmatch(line)
		if m == nil {
			p.addf(ln, "not a card entry: %s", quote(line))
			continue
		}
		idx, ok := atoiNonNeg(m[1])
		if !ok {
			p.addf(ln, "card index %s is not a number", quote(m[1]))
			continue
		}
		if seen[idx] {
			p.addf(ln, "card %d is listed twice; the first entry is kept", idx)
			continue
		}
		seen[idx] = true
		driver, name, found := strings.Cut(m[3], " - ")
		if !found {
			p.addf(ln, "card %d: no \" - \" between the driver and the name in %s", idx, quote(m[3]))
		}
		cards = append(cards, Card{
			Index:  idx,
			ID:     strings.TrimSpace(m[2]),
			Driver: strings.TrimSpace(driver),
			Name:   strings.TrimSpace(name),
		})
		pending, pendingLine = len(cards)-1, ln
	}
	unfinished()
	return cards, p.result()
}
