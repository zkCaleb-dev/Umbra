// Package view implements `umbra view`: fetch an account's
// confidential-token history from an Umbra instance and print the
// decrypted statement. Keys never leave this process and the server
// never decrypts — the API serves ciphertexts verbatim, and everything
// readable is opened locally with internal/ct.
package view

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/zkCaleb-dev/umbra/internal/ct"
)

// Config wires one `umbra view` invocation.
type Config struct {
	APIBase string
	RPCURL  string
	Token   string
	Secret  string // S… seed; never accepted as a flag
	Verify  bool
	Out     io.Writer
}

// Main parses CLI arguments and runs the statement. The secret comes
// from UMBRA_SECRET_KEY or an interactive prompt — never argv, which
// lands in shell history and process listings.
func Main(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("umbra view", flag.ContinueOnError)
	token := fs.String("token", "", "confidential token contract id (C…)")
	apiBase := fs.String("api", "https://umbra-production-d30f.up.railway.app", "umbra API base URL")
	rpcURL := fs.String("rpc", "https://soroban-testnet.stellar.org", "Soroban RPC URL for on-chain verification")
	verify := fs.Bool("verify", true, "reconcile the statement against on-chain commitments")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *token == "" {
		fs.Usage()
		return fmt.Errorf("--token is required")
	}

	secret := os.Getenv("UMBRA_SECRET_KEY")
	if secret == "" {
		fmt.Fprint(os.Stderr, "Secret key (S…, input is echoed): ")
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil {
			return fmt.Errorf("reading secret key: %w", err)
		}
		secret = strings.TrimSpace(line)
	}

	return Run(ctx, Config{
		APIBase: strings.TrimRight(*apiBase, "/"),
		RPCURL:  *rpcURL,
		Token:   *token,
		Secret:  secret,
		Verify:  *verify,
		Out:     os.Stdout,
	})
}

// Run derives the keys, fetches the history, replays it and prints the
// decrypted statement (plus the on-chain reconciliation when enabled).
func Run(ctx context.Context, cfg Config) error {
	keys, err := ct.DeriveKeys(cfg.Secret, cfg.Token)
	if err != nil {
		return err
	}

	events, err := fetchHistory(ctx, cfg.APIBase, cfg.Token, keys.Account)
	if err != nil {
		return err
	}

	st, err := ct.BuildStatement(keys.Account, keys.VK, events)
	if err != nil {
		return err
	}

	printStatement(cfg.Out, cfg, keys.Account, st)

	if cfg.Verify {
		verifyOnChain(ctx, cfg, keys, st)
	}
	return nil
}

// ===== history fetch =====

type historyItem struct {
	EventID        string          `json:"event_id"`
	Ledger         uint32          `json:"ledger"`
	LedgerClosedAt time.Time       `json:"ledger_closed_at"` // absent on older servers
	TxHash         string          `json:"tx_hash"`
	Kind           string          `json:"kind"`
	Addresses      []string        `json:"addresses"`
	AmountPublic   *string         `json:"amount_public"`
	Payload        json.RawMessage `json:"payload"`
}

type historyPage struct {
	Events     []historyItem `json:"events"`
	NextLedger *uint32       `json:"next_ledger"`
}

func fetchHistory(ctx context.Context, apiBase, token, account string) ([]ct.Event, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	var events []ct.Event
	since := uint32(0)
	for {
		u := fmt.Sprintf("%s/v1/ct/%s/history/%s?since_ledger=%d&limit=5000",
			apiBase, url.PathEscape(token), url.PathEscape(account), since)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("fetching history: %w", err)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		resp.Body.Close() //nolint:errcheck
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("history request: %s — %s", resp.Status, strings.TrimSpace(string(body)))
		}
		var page historyPage
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("decoding history: %w", err)
		}
		for _, it := range page.Events {
			ev, err := toEvent(it)
			if err != nil {
				return nil, fmt.Errorf("event %s: %w", it.EventID, err)
			}
			events = append(events, ev)
		}
		if page.NextLedger == nil {
			return events, nil
		}
		since = *page.NextLedger
	}
}

func toEvent(it historyItem) (ct.Event, error) {
	ev := ct.Event{
		ID: it.EventID, Ledger: it.Ledger, ClosedAt: it.LedgerClosedAt,
		TxHash: it.TxHash, Kind: it.Kind, Addresses: it.Addresses,
	}
	if it.AmountPublic != nil {
		n, ok := new(big.Int).SetString(*it.AmountPublic, 10)
		if !ok {
			return ev, fmt.Errorf("bad public amount %q", *it.AmountPublic)
		}
		ev.AmountPublic = n
	}
	p, err := ct.ParsePayload(it.Payload)
	if err != nil {
		return ev, err
	}
	ev.Payload = p
	return ev, nil
}

// ===== printing =====

// debits maps kind/role pairs whose amounts leave the balance.
var debits = map[string]bool{
	"deposit/sender": true, "withdraw/sender": true, "transfer/sender": true,
	"set_spender/owner": true,
}

var credits = map[string]bool{
	"deposit/recipient": true, "transfer/recipient": true,
	"spender_transfer/recipient": true, "revoke_spender/owner": true,
}

func printStatement(w io.Writer, cfg Config, account string, st *ct.Statement) {
	fmt.Fprintf(w, "confidential statement\n")
	fmt.Fprintf(w, "  token    %s\n", cfg.Token)
	fmt.Fprintf(w, "  account  %s\n", account)
	fmt.Fprintf(w, "  source   %s (server never decrypts)\n", cfg.APIBase)

	switch st.KeyState {
	case ct.KeyNone:
		fmt.Fprintf(w, "  view     public lifecycle — supply your viewing key to open private amounts\n\n")
	case ct.KeyMismatch:
		fmt.Fprintf(w, "  view     public lifecycle — this key does not match the account's on-chain\n")
		fmt.Fprintf(w, "           registration (the app that created it may derive keys differently)\n\n")
	default:
		fmt.Fprintf(w, "  view     private amounts opened with your key\n\n")
	}

	fmt.Fprintf(w, "  %-8s  %-16s  %-16s  %-9s  %18s  %s\n",
		"LEDGER", "TIME (UTC)", "KIND", "ROLE", "AMOUNT", "NOTE")
	for _, e := range st.Entries {
		when := "—"
		if !e.ClosedAt.IsZero() {
			when = e.ClosedAt.UTC().Format("2006-01-02 15:04")
		}
		fmt.Fprintf(w, "  %-8d  %-16s  %-16s  %-9s  %18s  %s\n",
			e.Ledger, when, e.Kind, e.Role, amountCell(e), e.Note)
	}

	fmt.Fprintf(w, "\nbalances\n")
	line := func(name string, v *big.Int) {
		if v == nil {
			hint := "locked (connect your viewing key)"
			if st.KeyState == ct.KeyMismatch {
				hint = "unavailable (key does not match this account)"
			} else if st.KeyState == ct.KeyMatch {
				hint = "unknown (no readable checkpoint yet)"
			}
			fmt.Fprintf(w, "  %-10s %s\n", name, hint)
			return
		}
		fmt.Fprintf(w, "  %-10s %s\n", name, ct.FormatAmount(v, ct.StellarAssetDecimals))
	}
	line("spendable", st.Spendable)
	line("pending", st.Pending)
	if st.Spendable != nil && st.Pending != nil {
		line("total", new(big.Int).Add(st.Spendable, st.Pending))
	}

	for _, n := range st.Notes {
		fmt.Fprintf(w, "\n  note: %s\n", n)
	}
}

// amountCell renders one entry's amount by visibility: signed number for
// public/decrypted, a word for the private/locked cases.
func amountCell(e ct.Entry) string {
	switch e.Visibility {
	case ct.VisPublic, ct.VisDecrypted:
		if e.Amount == nil {
			return "·"
		}
		human := ct.FormatAmount(e.Amount, ct.StellarAssetDecimals)
		switch {
		case debits[e.Kind+"/"+e.Role]:
			return "-" + human
		case credits[e.Kind+"/"+e.Role]:
			return "+" + human
		default:
			return human
		}
	case ct.VisPrivate:
		return "private"
	case ct.VisLocked:
		return "🔒 locked"
	default:
		return "·"
	}
}

// ===== on-chain verification =====

// verifyOnChain reconciles the decrypted statement against the
// contract's ConfidentialAccount entry: the derived PVK must equal the
// registered viewing key, and the accumulators must re-commit to
// C_spend / C_receive (SDK.md §10.6). Failures print loudly but do not
// abort — the statement is already on screen; the verdict qualifies it.
func verifyOnChain(ctx context.Context, cfg Config, keys *ct.Keys, st *ct.Statement) {
	w := cfg.Out
	fmt.Fprintf(w, "\non-chain verification (%s)\n", cfg.RPCURL)

	acct, latest, err := FetchConfidentialAccount(ctx, cfg.RPCURL, cfg.Token, keys.Account)
	if err != nil {
		fmt.Fprintf(w, "  unavailable: %v\n", err)
		return
	}
	fmt.Fprintf(w, "  latest ledger %d\n", latest)

	if !keys.PVK.Equal(acct.ViewingKey) {
		fmt.Fprintf(w, "  this viewing key is not the one registered on-chain for this account.\n")
		fmt.Fprintf(w, "  the public lifecycle above is correct; private amounts stay sealed until\n")
		fmt.Fprintf(w, "  opened with the key the account was registered with.\n")
		return
	}
	fmt.Fprintf(w, "  %-34s MATCH\n", "viewing key ↔ on-chain registration")
	if st.Spendable != nil {
		fmt.Fprintf(w, "  %-34s %s\n", "spendable ↔ C_spend commitment",
			verdict(ct.Commit(st.Spendable, st.SpendR).Equal(acct.Spendable)))
	}
	if st.Pending != nil {
		fmt.Fprintf(w, "  %-34s %s\n", "pending ↔ C_receive commitment",
			verdict(ct.Commit(st.Pending, st.ReceiveR).Equal(acct.Receiving)))
	}
}

func verdict(ok bool) string {
	if ok {
		return "MATCH"
	}
	return "MISMATCH"
}
