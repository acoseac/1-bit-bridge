package admin

import "net/http"

// pageUpload renders the Add-music page.
//
// Upload used to be a panel on the Roots page, below the roots table and the
// transcoded-cache card. It is a first-class action — on a host with no shell
// it is the ONLY way to get audio in — so it gets a rail entry and a page of
// its own rather than being something you scroll past.
//
// The rail entry is unconditional even when uploads are off, following the
// Smart Mixes precedent: the page is where the switch is, so hiding the entry
// would leave the feature undiscoverable from inside the console while
// Settings lists it two inches away.
func (s *Server) pageUpload(w http.ResponseWriter, r *http.Request) {
	s.renderPage(w, r, "upload", nil)
}
