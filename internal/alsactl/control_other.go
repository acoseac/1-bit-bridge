//go:build !linux || (!amd64 && !arm64)

package alsactl

import "errors"

func platformOpen(int) (Device, error) {
	return nil, errors.New("ALSA controls require Linux amd64 or arm64")
}
