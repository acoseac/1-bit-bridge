// Package installtest tests scripts/install.sh, the one-line installer a
// release attaches, and scripts/stamp-install.sh, which stamps it with the
// release's version. The tests run the real script under a POSIX shell
// against a local HTTPS server that serves fake releases, in a sandbox: a
// temporary HOME, a PATH of the real tools the script uses and stubs for the
// ones that would touch the system (systemctl, loginctl, sudo, su, uname,
// id), and a fake bridge program that checks its flags and records its
// calls, each word apart. The commands the installer prints are run as
// printed against a recorder of each word (testdata/argv), in place of every
// program they name. BRIDGE_INSTALL_TEST_SHELLS names the shells to run it
// under, comma separated (`make installer-check` lists every one the host
// has); the default is sh. They do not run on Windows, which the installer
// refuses.
package installtest
