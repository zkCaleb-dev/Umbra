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
	fmt.Fprintf(w, "  source   %s (server never decrypts)\n\n", cfg.APIBase)

	fmt.Fprintf(w, "  %-8s  %-16s  %-16s  %-9s  %16s  %s\n",
		"LEDGER", "TIME (UTC)", "KIND", "ROLE", "AMOUNT", "NOTE")
	for _, e := range st.Entries {
		when := "—"
		if !e.ClosedAt.IsZero() {
			when = e.ClosedAt.UTC().Format("2006-01-02 15:04")
		}
		amount := "·"
		if e.Amount != nil {
			switch {
			case debits[e.Kind+"/"+e.Role]:
				amount = "-" + e.Amount.String()
			case credits[e.Kind+"/"+e.Role]:
				amount = "+" + e.Amount.String()
			default:
				amount = e.Amount.String()
			}
		}
		fmt.Fprintf(w, "  %-8d  %-16s  %-16s  %-9s  %16s  %s\n",
			e.Ledger, when, e.Kind, e.Role, amount, e.Note)
	}

	fmt.Fprintf(w, "\nbalances (base units)\n")
	line := func(name string, v *big.Int) {
		if v == nil {
			fmt.Fprintf(w, "  %-10s unknown (no readable checkpoint)\n", name)
			return
		}
		fmt.Fprintf(w, "  %-10s %s\n", name, v.String())
	}
	line("spendable", st.Spendable)
	line("pending", st.Pending)
	if st.Spendable != nil && st.Pending != nil {
		line("total", new(big.Int).Add(st.Spendable, st.Pending))
	}

	for _, warn := range st.Warnings {
		fmt.Fprintf(w, "\n  warning: %s\n", warn)
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

	acct, latest, err := fetchConfidentialAccount(ctx, cfg.RPCURL, cfg.Token, keys.Account)
	if err != nil {
		fmt.Fprintf(w, "  unavailable: %v\n", err)
		return
	}
	fmt.Fprintf(w, "  latest ledger %d\n", latest)

	check := func(name string, ok bool) {
		verdict := "MATCH"
		if !ok {
			verdict = "MISMATCH — statement and chain disagree; re-run in case new events landed mid-fetch"
		}
		fmt.Fprintf(w, "  %-34s %s\n", name, verdict)
	}
	check("derived PVK vs registered key", keys.PVK.Equal(acct.viewingKey))
	if st.Spendable != nil {
		check("Commit(spendable) vs C_spend", ct.Commit(st.Spendable, st.SpendR).Equal(acct.spendable))
	}
	if st.Pending != nil {
		check("Commit(pending) vs C_receive", ct.Commit(st.Pending, st.ReceiveR).Equal(acct.receiving))
	}
}
