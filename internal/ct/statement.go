package ct

import (
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Statement replay per SDK.md §10.2/§10.9: events apply in emission
// order; checkpoint events (withdraw, sender-side transfer,
// set_spender, revoke_spender) OVERWRITE the spendable accumulator from
// their (b_tilde, sigma) — so a clamped history still converges on the
// spendable side — while the receiving accumulator replays additively
// and resets at each merge.
//
// Public-first: the lifecycle (kinds, roles, participants, public
// amounts) is built with or without a viewing key. A key only ADDS the
// private amounts on top; its absence or mismatch never breaks the
// statement — it just leaves the private amounts locked.

// Event is one history item, wire-format already parsed.
type Event struct {
	ID           string
	Ledger       uint32
	ClosedAt     time.Time
	TxHash       string
	Kind         string
	Addresses    []string
	AmountPublic *big.Int
	Payload      *Payload
}

// Visibility classifies what an entry's amount is:
//   - public:    on-chain in the clear (deposits, withdrawals)
//   - decrypted: read with the viewing key
//   - private:   belongs to another participant; not yours to read even
//     with a valid key (e.g. a transfer you sent — the amount is for the
//     recipient and auditors)
//   - locked:    private and yours, but no matching key was supplied
//   - none:      the event carries no amount (register, merge)
const (
	VisPublic    = "public"
	VisDecrypted = "decrypted"
	VisPrivate   = "private"
	VisLocked    = "locked"
	VisNone      = "none"
)

// Entry is one line of the statement.
type Entry struct {
	Ledger     uint32
	ClosedAt   time.Time
	Kind       string
	Role       string
	Amount     *big.Int // set only when Visibility is public or decrypted
	Visibility string
	Note       string
}

// Key state of a whole statement.
const (
	KeyNone     = "none"     // no viewing key supplied — public lifecycle only
	KeyMatch    = "match"    // key opened every private entry it should
	KeyMismatch = "mismatch" // key supplied but it does not fit this account
)

// Statement is one account's history, as readable as the key allows.
type Statement struct {
	Entries   []Entry
	KeyState  string
	Spendable *big.Int // nil unless the key opened the balance
	Pending   *big.Int
	// Blinding accumulators (mod q), for reconciling Commit(v, r)
	// against the on-chain C_spend / C_receive.
	SpendR, ReceiveR *big.Int
	Notes            []string // structural anomalies only, not key misses
}

// BuildStatement replays events for account. vk may be nil — then the
// statement is the public lifecycle with every private amount locked.
func BuildStatement(account string, vk *big.Int, events []Event) (*Statement, error) {
	ordered := make([]Event, 0, len(events))
	seen := make(map[string]bool, len(events))
	for _, ev := range events {
		if seen[ev.ID] {
			continue
		}
		seen[ev.ID] = true
		ordered = append(ordered, ev)
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		return eventOrder(ordered[i]) < eventOrder(ordered[j])
	})

	st := &Statement{KeyState: KeyNone}
	r := &replay{vk: vk, hasKey: vk != nil, account: account, st: st}
	for _, ev := range ordered {
		r.apply(ev)
	}
	if r.hasKey {
		st.KeyState = KeyMatch
		if r.mismatch {
			st.KeyState = KeyMismatch
		}
	}
	// Balances are only trustworthy when the key opened every checkpoint.
	if r.hasKey && !r.mismatch {
		if r.spendKnown {
			st.Spendable, st.SpendR = r.vSpend, r.rSpend
		}
		if r.receiveKnown {
			st.Pending, st.ReceiveR = r.vReceive, r.rReceive
		}
	}
	return st, nil
}

// eventOrder turns an "ledger-tx-index" event id into a sortable key.
func eventOrder(ev Event) uint64 {
	parts := strings.SplitN(ev.ID, "-", 3)
	if len(parts) != 3 {
		return uint64(ev.Ledger) << 32
	}
	l, _ := strconv.ParseUint(parts[0], 10, 32)
	tx, _ := strconv.ParseUint(parts[1], 10, 16)
	idx, _ := strconv.ParseUint(parts[2], 10, 16)
	return l<<32 | tx<<16 | idx
}

type replay struct {
	vk      *big.Int
	hasKey  bool
	account string
	st      *Statement

	vSpend, rSpend     *big.Int
	vReceive, rReceive *big.Int
	spendKnown         bool
	receiveKnown       bool
	mismatch           bool // a private entry failed to open under vk
}

func (r *replay) entry(ev Event, role, vis string, amount *big.Int, note string) {
	r.st.Entries = append(r.st.Entries, Entry{
		Ledger: ev.Ledger, ClosedAt: ev.ClosedAt, Kind: ev.Kind,
		Role: role, Amount: amount, Visibility: vis, Note: note,
	})
}

// tryCheckpoint overwrites the spendable accumulator from (b_tilde,
// sigma). Returns the previous value (for amount deltas) and whether the
// key opened it. Without a key it does nothing and reports locked.
func (r *replay) tryCheckpoint(ev Event) (prev *big.Int, opened bool) {
	if !r.hasKey || ev.Payload == nil || ev.Payload.BTilde == nil || ev.Payload.Sigma == nil {
		r.spendKnown = false
		return nil, false
	}
	v, err := DecryptBalance(r.vk, ev.Payload.Sigma, ev.Payload.BTilde)
	if err != nil {
		r.mismatch = true
		r.spendKnown = false
		return nil, false
	}
	if r.spendKnown {
		prev = r.vSpend
	}
	r.vSpend, r.rSpend, r.spendKnown = v, SpendRandomness(r.vk, ev.Payload.Sigma), true
	return prev, true
}

// tryCredit applies an inbound transfer to the receiving accumulator.
func (r *replay) tryCredit(ev Event, sigma *big.Int) (v *big.Int, opened bool) {
	if !r.hasKey || ev.Payload == nil || ev.Payload.RE == nil || ev.Payload.VTilde == nil || sigma == nil {
		r.receiveKnown = false
		return nil, false
	}
	s, err := SharedSecret(r.vk, ev.Payload.RE)
	if err != nil {
		r.mismatch = true
		return nil, false
	}
	v, err = DecryptAmount(s, sigma, ev.Payload.VTilde)
	if err != nil {
		r.mismatch = true
		r.receiveKnown = false
		return nil, false
	}
	if !r.receiveKnown {
		r.vReceive, r.rReceive, r.receiveKnown = big.NewInt(0), big.NewInt(0), true
	}
	r.vReceive = new(big.Int).Add(r.vReceive, v)
	r.rReceive = new(big.Int).Add(r.rReceive, TransferBlinding(s, sigma))
	r.rReceive.Mod(r.rReceive, FqModulus)
	return v, true
}

// lockedOrNone picks the visibility for a private amount the viewer is
// entitled to but could not read: locked (no/mismatched key).
func (r *replay) lockedVis() string { return VisLocked }

func (r *replay) apply(ev Event) {
	addr := func(i int) string {
		if i < len(ev.Addresses) {
			return ev.Addresses[i]
		}
		return ""
	}
	short := func(a string) string {
		if len(a) > 8 {
			return a[:4] + "…" + a[len(a)-4:]
		}
		return a
	}

	switch ev.Kind {
	case "register":
		if addr(0) != r.account {
			return
		}
		r.vSpend, r.rSpend = big.NewInt(0), big.NewInt(0)
		r.vReceive, r.rReceive = big.NewInt(0), big.NewInt(0)
		r.spendKnown, r.receiveKnown = true, true
		r.entry(ev, "owner", VisNone, nil, "account registered")

	case "deposit":
		from, to := addr(0), addr(1)
		if to == r.account {
			if !r.receiveKnown {
				r.vReceive, r.rReceive, r.receiveKnown = big.NewInt(0), big.NewInt(0), true
			}
			if ev.AmountPublic != nil { // deposits commit with zero blinding
				r.vReceive = new(big.Int).Add(r.vReceive, ev.AmountPublic)
			}
			r.entry(ev, "recipient", VisPublic, ev.AmountPublic, "public deposit into pending balance")
		} else if from == r.account {
			r.entry(ev, "sender", VisPublic, ev.AmountPublic, "funded a deposit to "+short(to))
		}

	case "merge":
		if addr(0) != r.account {
			return
		}
		if r.spendKnown && r.receiveKnown {
			r.vSpend = new(big.Int).Add(r.vSpend, r.vReceive)
			r.rSpend = new(big.Int).Add(r.rSpend, r.rReceive)
			r.rSpend.Mod(r.rSpend, FqModulus)
		} else {
			r.spendKnown = false
		}
		r.vReceive, r.rReceive, r.receiveKnown = big.NewInt(0), big.NewInt(0), true
		r.entry(ev, "owner", VisNone, nil, "pending balance folded into spendable")

	case "withdraw":
		from, to := addr(0), addr(1)
		if from != r.account {
			if to == r.account {
				r.entry(ev, "recipient", VisPublic, ev.AmountPublic, "received underlying tokens (outside the confidential balance)")
			}
			return
		}
		r.tryCheckpoint(ev) // updates spendable; the amount itself is public
		r.entry(ev, "sender", VisPublic, ev.AmountPublic, "withdrawn to underlying tokens for "+short(to))

	case "transfer":
		from, to := addr(0), addr(1)
		if from == r.account {
			prev, opened := r.tryCheckpoint(ev)
			if opened && prev != nil {
				r.entry(ev, "sender", VisDecrypted, new(big.Int).Sub(prev, r.vSpend), "sent to "+short(to))
			} else {
				r.entry(ev, "sender", r.lockedVis(), nil, "sent to "+short(to))
			}
		}
		if to == r.account {
			if v, opened := r.tryCredit(ev, sigmaOf(ev)); opened {
				r.entry(ev, "recipient", VisDecrypted, v, "received from "+short(from))
			} else {
				r.entry(ev, "recipient", r.lockedVis(), nil, "received from "+short(from))
			}
		}

	case "set_spender":
		account, spender := addr(0), addr(1)
		if account != r.account {
			if spender == r.account {
				r.entry(ev, "spender", VisNone, nil, "granted an allowance by "+short(account))
			}
			return
		}
		prev, opened := r.tryCheckpoint(ev)
		note := "escrowed an allowance to " + short(spender)
		if ev.Payload != nil && ev.Payload.LiveUntilLedger > 0 {
			note += fmt.Sprintf(" (expires ledger %d)", ev.Payload.LiveUntilLedger)
		}
		if opened && prev != nil {
			r.entry(ev, "owner", VisDecrypted, new(big.Int).Sub(prev, r.vSpend), note)
		} else {
			r.entry(ev, "owner", r.lockedVis(), nil, note)
		}

	case "revoke_spender":
		account, spender := addr(0), addr(1)
		if account != r.account {
			if spender == r.account {
				r.entry(ev, "spender", VisNone, nil, "allowance revoked by "+short(account))
			}
			return
		}
		prev, opened := r.tryCheckpoint(ev)
		if opened && prev != nil {
			r.entry(ev, "owner", VisDecrypted, new(big.Int).Sub(r.vSpend, prev), "reclaimed unspent allowance from "+short(spender))
		} else {
			r.entry(ev, "owner", r.lockedVis(), nil, "reclaimed unspent allowance from "+short(spender))
		}

	case "spender_transfer":
		spender, from, to := addr(0), addr(1), addr(2)
		if to == r.account {
			if v, opened := r.tryCredit(ev, sigmaAOf(ev)); opened {
				r.entry(ev, "recipient", VisDecrypted, v, "received from "+short(from)+" via spender "+short(spender))
			} else {
				r.entry(ev, "recipient", r.lockedVis(), nil, "received from "+short(from)+" via spender "+short(spender))
			}
		}
		if from == r.account && to != r.account {
			// The amount is encrypted to the recipient and auditors only —
			// intentionally not readable by the sender. That is the
			// protocol, not a missing key.
			r.entry(ev, "owner", VisPrivate, nil,
				"spender "+short(spender)+" paid "+short(to)+" from the escrowed allowance")
		}
		if spender == r.account && from != r.account && to != r.account {
			r.entry(ev, "spender", VisPrivate, nil, "spent from "+short(from)+"'s allowance to "+short(to))
		}

	default:
		r.entry(ev, "", VisNone, nil, "unrecognized event kind (ignored)")
	}
}

func sigmaOf(ev Event) *big.Int {
	if ev.Payload == nil {
		return nil
	}
	return ev.Payload.Sigma
}

func sigmaAOf(ev Event) *big.Int {
	if ev.Payload == nil {
		return nil
	}
	return ev.Payload.SigmaA
}
