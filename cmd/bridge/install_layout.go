package main

import (
	"context"
	"fmt"
	"io"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/doctor"
)

// The bridge's own files inside a library root (backlog B884): the config
// file, or the data directory, inside a root whose mount stops answering
// takes every command that reads them down with it, serve, the doctor and
// status among them. Three places say so, from one check (config.LayoutOf,
// which touches nothing, so asking it cannot wait on such a mount) and in one
// set of words (config.InstallLayout's): bridge init when it writes a config
// or keeps one, bridge doctor and the console's doctor in a line of their
// own, and serve in its journal at each start. None of them refuses: an
// install keeps working whatever its layout.

// installLayoutCheckName is the doctor's line on the config file and the data
// directory inside a library root, shown only where it warns.
const installLayoutCheckName = "install-layout"

// warnInstallLayout is bridge init's one line on the layout it wrote or kept:
// the config at cfgPath, its data directory dataDir and its library roots,
// as the file says them, in the CLI's words (config.InstallLayout.Warning),
// with the one period that ends it. Nothing for a layout with neither inside
// a root, so a healthy run's output is as it was. init's preflight carries no
// such line (initPreflightDeps), so a run says it once whether or not the
// preflight runs.
func warnInstallLayout(w io.Writer, cfgPath, dataDir string, roots []string) {
	l := config.LayoutOf(cfgPath, dataDir, roots)
	if !l.Inside() {
		return
	}
	fmt.Fprintf(w, "warning: %s.\n", l.Warning())
}

// doctorInstallLayout is doctor.Deps.InstallLayout for the config at cfgFile,
// which loaded as cfg: one warn naming the config file or the data directory
// inside a library root, by path, with the root's place in libraryRoots, its
// hint the CLI's words and where the steps are, or no line at all. The
// console's doctor builds it from the config it loads at each run
// (buildDoctorDepsWith), so a root added while serve runs is there; its
// report is a success body, where the console shows the paths the operator
// configured, as its data-dir and library-roots lines do.
func doctorInstallLayout(cfgFile string, cfg *config.Config) func(context.Context) []doctor.Check {
	return func(context.Context) []doctor.Check {
		l := config.LayoutOf(cfgFile, cfg.DataDir, cfg.LibraryRoots)
		if !l.Inside() {
			return nil
		}
		return []doctor.Check{{Name: installLayoutCheckName, Status: doctor.Warn,
			Summary: l.Summary(true), Hint: l.Hint()}}
	}
}

// logInstallLayout is serve's journal line on the layout it starts from, once
// at the start, logged at Warn where the config file at cfgFile or cfg's data
// directory lies inside a library root. It names the root by its place in
// libraryRoots and the data directory by its setting, and no path, as every
// other line about the config in the journal does (B505). A root added while
// serve runs is the doctor's to say.
func logInstallLayout(cfgFile string, cfg *config.Config) {
	if l := config.LayoutOf(cfgFile, cfg.DataDir, cfg.LibraryRoots); l.Inside() {
		logger.Warn(l.JournalLine())
	}
}
