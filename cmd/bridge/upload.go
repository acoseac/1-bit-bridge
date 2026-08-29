package main

import "time"

// uploadSweepInterval is how often abandoned upload-staging directories are
// reaped. The sweeper also runs once at startup — a crash leaves orphaned
// .part files, and a ticker-only sweeper would leave them for a full period.
const uploadSweepInterval = time.Hour
