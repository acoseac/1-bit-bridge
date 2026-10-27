package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/alsactl"
	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
	"github.com/acoseac/1-bit-bridge/internal/player/qualification"
)

type playReportOptions struct{ report, contact, mixer string }

func playReportFlags(fs *flag.FlagSet) *playReportOptions {
	o := new(playReportOptions)
	fs.StringVar(&o.report, "report", "", "new approved-only production playback report")
	fs.StringVar(&o.contact, "contact-report", "", "hashed contact bundle for all DSD tuples")
	fs.StringVar(&o.mixer, "mixer-snapshot", "", "fresh amixer annex")
	return o
}
func (o *playReportOptions) validate() error {
	n := 0
	for _, s := range []string{o.report, o.contact, o.mixer} {
		if s != "" {
			n++
		}
	}
	if n != 0 && n != 3 {
		return errors.New("--report, --contact-report and --mixer-snapshot are required together")
	}
	return nil
}

type contactReceipt struct {
	Contact qualification.Contact
	SHA256  string
}
type contactBundle struct{ Receipts []contactReceipt }

func readContactBundle(path string) (contactBundle, error) {
	var v contactBundle
	b, e := qualification.ReadEvidenceFile(path)
	if e != nil {
		return v, e
	}
	if e = json.Unmarshal(b, &v); e != nil {
		return v, e
	}
	if len(v.Receipts) == 0 || len(v.Receipts) > 64 {
		return v, errors.New("contact bundle bounds")
	}
	for _, r := range v.Receipts {
		if r.SHA256 != qualification.Digest(r.Contact) {
			return v, errors.New("contact receipt hash mismatch")
		}
	}
	return v, nil
}

type playReport struct {
	mu                   sync.Mutex
	file                 *os.File
	Scenario             string
	QualificationVersion int
	End                  string
	Notes                []playback.Note
	Before, After        []alsactl.Snapshot
	AmixerAnnex          string
}

func (o *playReportOptions) prepare(a *playback.Authorization, trees outputTrees) (*playReport, error) {
	if o.report == "" {
		return nil, nil
	}
	if _, e := readContactBundle(o.contact); e != nil {
		return nil, e
	}
	annex, e := qualification.ReadEvidenceFile(o.mixer)
	if e != nil || len(annex) == 0 {
		return nil, errors.New("mixer annex missing")
	}
	f, e := os.OpenFile(o.report, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return nil, e
	}
	r := &playReport{file: f, Scenario: "production", QualificationVersion: qualification.Version, End: "incomplete", AmixerAnnex: string(annex)}
	a.ContactCheck = func(n alsapcm.NodeInfo, p qualification.ApprovedPolicy, manifest string) error {
		bundle, e := readContactBundle(o.contact)
		if e != nil {
			return e
		}
		sys := alsa.SysfsReader{Root: trees.sysfs}
		q, e := sys.Qualification(n.Card)
		if e != nil {
			return e
		}
		boot, e := os.ReadFile(filepath.Join(trees.proc, "sys/kernel/random/boot_id"))
		if e != nil {
			return e
		}
		controls, e := (alsactl.Reader{}).Read(n.Card)
		if e != nil {
			return e
		}
		attachment := qualification.Attachment{BootID: strings.TrimSpace(string(boot)), Port: q.Port, Manufacturer: q.Manufacturer, Product: q.Product, BCDDevice: q.BCDDevice, Card: n.Card, DevNum: q.DevNum, Interface: q.Interface}
		if !strings.HasPrefix(r.AmixerAnnex, fmt.Sprintf("# card=%d\n", n.Card)) {
			return errors.New("mixer annex card mismatch")
		}
		for _, receipt := range bundle.Receipts {
			if receipt.Contact.Tuple == p.Tuple {
				if e := receipt.Contact.Validate(p.Tuple, attachment, n.Device, n.Subdevice, controls.Hash(), manifest); e != nil {
					return e
				}
				r.mu.Lock()
				r.Before = append(r.Before, controls)
				r.mu.Unlock()
				return nil
			}
		}
		return errors.New("contact for approved tuple missing")
	}
	return r, nil
}
func (r *playReport) note(n playback.Note) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.Notes = append(r.Notes, n)
	r.mu.Unlock()
}
func (r *playReport) finish(code int) error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	defer r.file.Close()
	seen := map[int]bool{}
	for _, before := range r.Before {
		if seen[before.Card] {
			continue
		}
		seen[before.Card] = true
		after, err := (alsactl.Reader{}).Read(before.Card)
		if err != nil {
			code = 1
		} else {
			r.After = append(r.After, after)
		}
	}
	if code == 0 {
		r.End = "finished"
	}
	return json.NewEncoder(r.file).Encode(r)
}
