//go:build windows

package dlna

import (
	"testing"

	"golang.org/x/sys/windows"
)

// TestWSAPktinfoConstantsAreWinsocks pins the level and type parseWSAPktinfo
// reads to x/sys/windows' IPPROTO_IP and IP_PKTINFO, the option
// armDatagramDst sets.
func TestWSAPktinfoConstantsAreWinsocks(t *testing.T) {
	if wsaIPProtoIP != windows.IPPROTO_IP || wsaIPPktinfo != windows.IP_PKTINFO {
		t.Errorf("parseWSAPktinfo reads level %d type %d, Winsock's IPPROTO_IP and IP_PKTINFO are %d and %d",
			wsaIPProtoIP, wsaIPPktinfo, windows.IPPROTO_IP, windows.IP_PKTINFO)
	}
}
