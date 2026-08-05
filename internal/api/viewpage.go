package api

import (
	"context"
	"embed"
	"encoding/base64"
	"net/http"
	"time"

	"github.com/zkCaleb-dev/umbra/internal/config"
	"github.com/zkCaleb-dev/umbra/internal/view"
)

// The /view page: internal/ct compiled to WASM, decrypting statements
// entirely in the visitor's browser. The server's only roles are
// serving the static page, the ciphertext history it already serves,
// and one public on-chain record for client-side verification.

//go:embed view.html
var viewHTML []byte

// viewAssets holds the built WASM module + Go runtime shim. Built by
// `make wasm` (and the Dockerfile); absent in a plain `go build`, in
// which case the page reports how to build it.
//
//go:embed all:viewassets
var viewAssets embed.FS

func (s *Server) handleViewPage(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(viewHTML)
}

func (s *Server) handleViewAsset(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("asset")
	data, err := viewAssets.ReadFile("viewassets/" + name)
	if err != nil {
		http.Error(w, "asset not built — run `make wasm` before building the server", http.StatusNotFound)
		return
	}
	switch name {
	case "umbra.wasm":
		w.Header().Set("Content-Type", "application/wasm")
	case "wasm_exec.js":
		w.Header().Set("Content-Type", "application/javascript")
	default:
		http.Error(w, "unknown asset", http.StatusNotFound)
		return
	}
	// The module is versioned by deploy; a day of caching keeps reloads
	// instant without pinning stale crypto for long.
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(data)
}

// handleCTAccount proxies the PUBLIC ConfidentialAccount record
// (registered viewing key + balance commitments) so the browser page
// can verify its decryption against the chain without an RPC of its
// own. No secrets involved: anyone can read this straight from any RPC.
func (s *Server) handleCTAccount(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	if kind, watched := s.watchedContract(token); !watched || kind != config.KindConfidentialToken {
		http.Error(w, "confidential token not indexed", http.StatusNotFound)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()

	var acct *view.OnChainAccount
	var latest uint32
	var err error
	for _, rpcURL := range s.cfg.RPCURLs {
		if acct, latest, err = view.FetchConfidentialAccount(ctx, rpcURL, token, r.PathValue("address")); err == nil {
			break
		}
	}
	if err != nil {
		// "not registered" is the common, user-meaningful case.
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	b64 := base64.StdEncoding.EncodeToString
	s.ok(w, map[string]any{
		"latest_ledger":        latest,
		"viewing_public_key":   b64(acct.ViewingKey.Bytes()),
		"spendable_commitment": b64(acct.Spendable.Bytes()),
		"receiving_commitment": b64(acct.Receiving.Bytes()),
	})
}
