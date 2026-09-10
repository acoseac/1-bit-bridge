package admin

import (
	"net/http"

	"github.com/acoseac/1-bit-bridge/internal/config"
)

// Managed controls — operator ACTIONS a control plane owns.
//
// `deployment.managedSettings` already covers settings fields: the
// console hides them and PATCH refuses them. This is the same idea one
// level over, for the things that are buttons rather than values.
//
// The console hiding is a courtesy. The refusal here is the boundary:
// every managed control is a plain authenticated request that anyone
// holding a console session can send by hand, and on the hosted product
// the person holding that session is a customer, not the operator of
// the machine.
//
// The gate is applied at the ROUTE TABLE rather than inside each
// handler, because the route table is where you go to ask "what can
// somebody with a session do here" — a check buried forty lines into a
// handler answers that question only if you already suspected it.
// TestManagedControlsAreEnforcedAtTheRouteTable parses the registrations and
// requires the set of routes actually wrapped here to EQUAL the set the
// behavioural test drives — so a managed route cannot be added without
// coverage, and one cannot be removed leaving a row that passes for some other
// reason. It does NOT claim to notice a route that SHOULD be managed and is
// not; nothing can decide that from source, and this comment claimed it for
// months while no test of that name existed at all.

// managedControlMessages says WHY the control is absent, in the terms of
// the person who just pressed it. "Forbidden" answers a question nobody
// asked; what they want to know is whether the thing they wanted is
// going to happen, and who does it.
var managedControlMessages = map[string]string{
	config.ManagedControlRestart:     "Restarting this bridge is handled by the host it runs on.",
	config.ManagedControlUpdates:     "The bridge version on this host is managed centrally, and updates arrive with the host's own.",
	config.ManagedControlRoots:       "Where this library reads from is set by the host. Add music through Upload instead.",
	config.ManagedControlVariantsDir: "Where converted files are written is set by the host.",
	config.ManagedControlBackups:     "The host takes this bridge's backups, and is the only place one can be restored from.",
}

// managedControl reports whether an action is control-plane owned here.
// Reads the live config per call — `managedControls` is not hot-appliable
// today, but nothing about this gate wants a boot-time snapshot, and a
// stale answer here would be a stale answer about a security boundary.
func (s *Server) managedControl(name string) bool {
	if s == nil || s.deps.CfgHolder == nil {
		return false
	}
	cfg := s.deps.CfgHolder.Load()
	if cfg == nil {
		return false
	}
	return cfg.Deployment.IsManagedControl(name)
}

// managed wraps a handler so a control-plane-owned action is refused.
//
// 403, not 404: the endpoint exists and the caller is authenticated —
// the answer is "not by you, and not from here", which is what 403
// means. A 404 would send an operator debugging a real problem looking
// for a routing fault.
func (s *Server) managed(name string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.managedControl(name) {
			msg := managedControlMessages[name]
			if msg == "" {
				msg = "This control is managed by the host that runs this bridge."
			}
			writeError(w, http.StatusForbidden, "managed_by_host", msg)
			return
		}
		next(w, r)
	}
}

// managedDeployment reports whether a control plane runs this bridge —
// see config.DeploymentConfig.IsManaged. Used by the surfaces that would
// otherwise hand the reader an instruction requiring a shell on the host.
func (s *Server) managedDeployment() bool {
	if s == nil || s.deps.CfgHolder == nil {
		return false
	}
	cfg := s.deps.CfgHolder.Load()
	if cfg == nil {
		return false
	}
	return cfg.Deployment.IsManaged()
}

// managedControlSet renders the configured controls as a map so a
// template can ask `{{if .Managed.roots}}` without a FuncMap helper.
//
// Every known control gets a key, present or not, so a template naming
// one that is not managed reads `false` rather than the zero value of a
// missing key — which is the same thing here, but stops being so the
// moment somebody writes `{{if not .Managed.typo}}` and gets a control
// that is always shown.
func managedControlSet(cfg *config.Config) map[string]bool {
	out := make(map[string]bool, len(config.KnownManagedControls()))
	for _, name := range config.KnownManagedControls() {
		out[name] = cfg != nil && cfg.Deployment.IsManagedControl(name)
	}
	return out
}
