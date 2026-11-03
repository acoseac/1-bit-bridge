// Package approvaltest prepares private directories for owner-approval
// fixtures. Approval records and their evidence are read only from a
// directory no other user can write, and a test's temporary directory takes
// its mode from the umask (0002 leaves it group-writable), so SecureDir and
// TempDir set 0700 before a fixture writes there.
package approvaltest
