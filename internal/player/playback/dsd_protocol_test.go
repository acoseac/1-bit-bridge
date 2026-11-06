package playback_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
)

// TestEveryDSDRefusalWordIsInTheProtocol (B728 B7): every word the
// effective policy refuses a rate with, read from its own definitions
// (policy.go's Reason constants), and the backend's two of its own, a
// converted rate's dsd_not_in_this_release and no set's dsd_dac_unread, are
// named in PROTOCOL.md's paragraph on dsdPlayback.reason, which publishes the
// lowest rate's word while no rate plays.
func TestEveryDSDRefusalWordIsInTheProtocol(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	doc, err := os.ReadFile(filepath.Join(root, "PROTOCOL.md"))
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(doc), "`dsdPlayback.reason` is for display only")
	if start < 0 {
		t.Fatal("PROTOCOL.md has no paragraph on dsdPlayback.reason")
	}
	paragraph := string(doc[start:])
	if end := strings.Index(paragraph, "\n\n"); end >= 0 {
		paragraph = paragraph[:end]
	}
	f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(root, "internal", "player", "policy", "policy.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	words := []string{string(playback.ReasonDSD), string(playback.ReasonDACUnread)}
	ast.Inspect(f, func(n ast.Node) bool {
		v, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for i, name := range v.Names {
			if !strings.HasPrefix(name.Name, "Reason") || i >= len(v.Values) {
				continue
			}
			switch x := v.Values[i].(type) {
			case *ast.BasicLit:
				w, err := strconv.Unquote(x.Value)
				if err != nil {
					t.Fatal(err)
				}
				words = append(words, w)
			case *ast.SelectorExpr:
				if x.Sel.Name != "RefusalStateUnreadable" {
					t.Fatalf("%s is %s.%s, which this test does not resolve", name.Name, x.X, x.Sel.Name)
				}
				words = append(words, dsdstate.RefusalStateUnreadable)
			default:
				t.Fatalf("%s's value %T does not resolve", name.Name, x)
			}
		}
		return true
	})
	if len(words) < 12 {
		t.Fatalf("read %d refusal words from the policy: %v", len(words), words)
	}
	for _, w := range words {
		if !strings.Contains(paragraph, "`"+w+"`") {
			t.Errorf("PROTOCOL.md's dsdPlayback.reason paragraph does not name %s", w)
		}
	}
}
