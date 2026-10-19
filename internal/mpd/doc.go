// Package mpd is a read-only client for the Music Player Daemon, and the
// reading of the player that daemon is part of.
//
// The client dials a TCP address or a unix socket, reads the greeting, and
// sends only the commands in allowedCommands. There is no method that
// starts, stops or seeks playback, and no method that edits the queue or
// the database: exchange refuses a command that is not on that list before
// anything is written. password is on the list only so a caller who already
// holds one can authenticate, and the value is not kept, logged or placed
// in an error. A failed Dial returns no Client, so the bufio.Reader that
// may have read an ACK echoing the password does not outlive the call.
//
// config is one of the read commands. Over a TCP connection MPD answers it
// with ACK [4@0] {config} Command only permitted to local clients, so a
// music_directory learned over TCP comes from the configuration file
// (package conf), not from that reply. A unix socket is the connection
// MPD treats as local.
//
// Nothing here is a wire type. bridge output player prints a Report; a
// later /v1 route that serves one wraps it in its own DTO.
package mpd
