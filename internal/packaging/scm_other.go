//go:build !windows

package packaging

// hostServiceManager off Windows has no calls: there is no service manager to
// ask, so the install check tells nothing (errNoServiceManager).
var hostServiceManager = serviceManager{}
