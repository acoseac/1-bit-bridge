package main

import "time"

// uploadSweepInterval is how often abandoned upload-staging directories are
// reaped. The sweeper also runs once at startup — a crash leaves orphaned
// .part files, and a ticker-only sweeper would leave them for a full period.
const uploadSweepInterval = time.Hour

// trashSweepInterval is how often expired trash is purged. Like the upload
// sweeper it also runs once at startup, so a bridge that was down past a
// deletion's TTL reclaims it on the way up rather than a period later.
const trashSweepInterval = time.Hour
