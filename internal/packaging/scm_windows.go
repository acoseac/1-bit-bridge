//go:build windows

package packaging

import "golang.org/x/sys/windows"

// hostServiceManager is Windows's service manager, as the install check asks
// it: the access it asks for is the check's, never all access, which is what
// lets a standard user's bridge logs see the service.
var hostServiceManager = serviceManager{
	connect: func(access uint32) (uintptr, error) {
		h, err := windows.OpenSCManager(nil, nil, access)
		return uintptr(h), err
	},
	open: func(manager uintptr, name string, access uint32) (uintptr, error) {
		p, err := windows.UTF16PtrFromString(name)
		if err != nil {
			return 0, err
		}
		h, err := windows.OpenService(windows.Handle(manager), p, access)
		return uintptr(h), err
	},
	closeHandle: func(h uintptr) {
		_ = windows.CloseServiceHandle(windows.Handle(h))
	},
	notRegistered: isServiceMissing,
}
