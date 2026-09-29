package enrich

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"sort"
	"strings"
	"testing"
)

// requestBuilders lists every function in this package that builds an HTTP
// request from a URL string, and why that is safe. A request built from a
// URL an operator wrote (`enrich.musicbrainzBaseURL`, `coverArtBaseURL`) must
// be built by baseEndpoint.newRequest, which keeps the base's user
// information out of the request URL and so out of every error net/http
// returns for it (backlog B69). The others fetch a public constant, or a URL
// a public service's own response named.
var requestBuilders = map[string]string{
	"(baseEndpoint).newRequest":    "the one builder for a base an operator configures: MusicBrainz, Cover Art, and the Atlas premium cover fetch",
	"(*DeezerClient).SearchArtist": "DefaultDeezerBase, a constant no operator URL reaches",
	"(*DeezerClient).FetchImage":   "a picture URL Deezer's search named, refused unless its host is Deezer's",
	"(*ITunesClient).get":          "DefaultITunesBase, a constant no operator URL reaches",
	"(*ITunesClient).FetchArtwork": "an artwork URL the iTunes search response named",
}

// TestEveryRequestThisPackageBuildsComesFromAListedBuilder pins the
// population, not the clients: the defect behind backlog B69 was a client
// built from a configured URL that put the URL, credential and all, into its
// requests, and the next client to be added is where it comes back. The guard
// parses this package's own non-test files and requires the set of functions
// that call http.NewRequest, http.NewRequestWithContext or the package-level
// Get, Head, Post and PostForm to equal requestBuilders, in both directions,
// so a builder nobody listed and an entry nothing builds from both fail.
func TestEveryRequestThisPackageBuildsComesFromAListedBuilder(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	builders := map[string]bool{}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		// The go tool ignores a name starting with "." or "_", so no build
		// sees the file and nothing it holds is this package's.
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") ||
			strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, d := range f.Decls {
			key := "(package level)"
			if fd, ok := d.(*ast.FuncDecl); ok {
				key = funcKey(fd)
			}
			ast.Inspect(d, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok && buildsARequest(call) {
					builders[key] = true
				}
				return true
			})
		}
	}

	var unlisted, stale []string
	for key := range builders {
		if _, ok := requestBuilders[key]; !ok {
			unlisted = append(unlisted, key)
		}
	}
	for key := range requestBuilders {
		if !builders[key] {
			stale = append(stale, key)
		}
	}
	sort.Strings(unlisted)
	sort.Strings(stale)
	if len(unlisted) > 0 {
		t.Errorf("%v build an HTTP request and are not in requestBuilders. If the URL is one an operator "+
			"configures, build the request with baseEndpoint.newRequest, so its user information never enters "+
			"the request URL or an error naming it. If it is a public constant, list it with the reason.", unlisted)
	}
	if len(stale) > 0 {
		t.Errorf("requestBuilders lists %v, which build no request: remove the entry (or the guard reads nothing)", stale)
	}
}

// funcKey names a function declaration as requestBuilders spells it:
// `(*ITunesClient).get`, `(baseEndpoint).newRequest`, or a bare name.
func funcKey(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	return "(" + types.ExprString(fd.Recv.List[0].Type) + ")." + fd.Name.Name
}

// buildsARequest reports whether call is net/http's own constructor of a
// request from a URL string: http.NewRequest, http.NewRequestWithContext, or
// one of the package-level helpers that build one on the default client.
func buildsARequest(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != "http" {
		return false
	}
	switch sel.Sel.Name {
	case "NewRequest", "NewRequestWithContext", "Get", "Head", "Post", "PostForm":
		return true
	}
	return false
}

// credentialSenders lists every function in this package that calls
// baseEndpoint.newRequest, and so sends a credential an operator wrote to the
// host it names, beside the constructor that builds that client's http.Client
// over guardRedirects. net/http would carry the credential from an https
// request onto a plain-http hop on the same host, and the guard is what
// withholds it there.
var credentialSenders = map[string]string{
	"(*MusicBrainzClient).get":              "NewMusicBrainzClient",
	"(*CoverArtClient).fetchStream":         "NewCoverArtClient",
	"(*atlasPremiumFetcher).authedCoverGet": "NewAtlasPremiumFetcher",
}

// TestEveryClientThatSendsACredentialIsBuiltWithTheRedirectGuard pins the
// population behind TestNoCredentialFollowsARedirectFromHTTPSToACleartextHop,
// which drives the three clients that exist. A fourth client that builds its
// requests through newRequest sends a credential too, and builds no request
// through http.NewRequest, so the list above is where it is found: the set of
// functions that call newRequest must equal credentialSenders, in both
// directions, and each listed constructor must call guardRedirects.
func TestEveryClientThatSendsACredentialIsBuiltWithTheRedirectGuard(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	senders := map[string]bool{}
	constructors := map[string]*ast.FuncDecl{}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") ||
			strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			if fd.Recv == nil {
				constructors[fd.Name.Name] = fd
			}
			if callsNamed(fd, func(call *ast.CallExpr) bool {
				sel, ok := call.Fun.(*ast.SelectorExpr)
				return ok && sel.Sel.Name == "newRequest"
			}) {
				senders[funcKey(fd)] = true
			}
		}
	}

	var unlisted, stale []string
	for key := range senders {
		if _, ok := credentialSenders[key]; !ok {
			unlisted = append(unlisted, key)
		}
	}
	for key := range credentialSenders {
		if !senders[key] {
			stale = append(stale, key)
		}
	}
	sort.Strings(unlisted)
	sort.Strings(stale)
	if len(unlisted) > 0 {
		t.Errorf("%v call baseEndpoint.newRequest and are not in credentialSenders. They send a credential an "+
			"operator wrote, so their client must be built over guardRedirects, which withholds it from a hop "+
			"that leaves https: list the sender with its constructor.", unlisted)
	}
	if len(stale) > 0 {
		t.Errorf("credentialSenders lists %v, which call no newRequest: remove the entry (or the guard reads nothing)", stale)
	}

	for sender, ctor := range credentialSenders {
		fd, ok := constructors[ctor]
		if !ok {
			t.Errorf("credentialSenders names %s as the constructor for %s, and no such function exists", ctor, sender)
			continue
		}
		if !callsNamed(fd, func(call *ast.CallExpr) bool {
			id, ok := call.Fun.(*ast.Ident)
			return ok && id.Name == "guardRedirects"
		}) {
			t.Errorf("%s builds the client for %s and does not call guardRedirects: net/http would send its "+
				"credential to a plain-http hop on the same host", ctor, sender)
		}
	}
}

// callsNamed reports whether the function declaration fd holds a call that
// match accepts.
func callsNamed(fd *ast.FuncDecl, match func(*ast.CallExpr) bool) bool {
	found := false
	ast.Inspect(fd, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok && match(call) {
			found = true
		}
		return !found
	})
	return found
}
