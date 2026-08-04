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

// Entry is one line of the decrypted statement.
type Entry struct {
	Ledger   uint32
	ClosedAt time.Time
	Kind     string
	Role     string
	Amount   *big.Int // nil when this key cannot read it
	Note     string
}

// Statement is the decrypted view of one account's history.
type Statement struct {
	Entries   []Entry
	Spendable *big.Int // nil if never anchored
	Pending   *big.Int
	// Blinding accumulators (mod q), for reconciling Commit(v, r)
	// against the on-chain C_spend / C_receive.
	SpendR, ReceiveR *big.Int
	Warnings         []string
}

// BuildStatement replays events for account under vk. Events may arrive
// in any order and with duplicates; emission order (ledger, tx, index)
// is reconstructed from the event id.
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

	st := &Statement{}
	r := &replay{vk: vk, account: account, st: st}
	for _, ev := range ordered {
		if err := r.apply(ev); err != nil {
			return nil, fmt.Errorf("event %s (%s): %w", ev.ID, ev.Kind, err)
		}
	}
	if r.spendKnown {
		st.Spendable, st.SpendR = r.vSpend, r.rSpend
	}
	if r.receiveKnown {
		st.Pending, st.ReceiveR = r.vReceive, r.rReceive
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
	account string
	st      *Statement

	vSpend, rSpend     *big.Int
	vReceive, rReceive *big.Int
	spendKnown         bool
	receiveKnown       bool
}

func (r *replay) warnf(format string, args ...any) {
	r.st.Warnings = append(r.st.Warnings, fmt.Sprintf(format, args...))
}

func (r *replay) entry(ev Event, role string, amount *big.Int, note string) {
	r.st.Entries = append(r.st.Entries, Entry{
		Ledger: ev.Ledger, ClosedAt: ev.ClosedAt, Kind: ev.Kind,
		Role: role, Amount: amount, Note: note,
	})
}

// checkpoint overwrites the spendable accumulator from (b_tilde, sigma)
// and returns the previous value when it was known (for amount deltas).
func (r *replay) checkpoint(ev Event) (prev *big.Int, err error) {
	if ev.Payload == nil || ev.Payload.BTilde == nil || ev.Payload.Sigma == nil {
		return nil, fmt.Errorf("missing b_tilde/sigma")
	}
	v, err := DecryptBalance(r.vk, ev.Payload.Sigma, ev.Payload.BTilde)
	if err != nil {
		return nil, err
	}
	if r.spendKnown {
		prev = r.vSpend
	}
	r.vSpend = v
	r.rSpend = SpendRandomness(r.vk, ev.Payload.Sigma)
	r.spendKnown = true
	return prev, nil
}

// credit applies an inbound transfer to the receiving accumulator.
// sigma is the amount salt — the event's sigma, or sigma_a for
// spender_transfer.
func (r *replay) credit(ev Event, sigma *big.Int) (*big.Int, error) {
	if ev.Payload == nil || ev.Payload.RE == nil || ev.Payload.VTilde == nil || sigma == nil {
		return nil, fmt.Errorf("missing r_e/v_tilde/salt")
	}
	s, err := SharedSecret(r.vk, ev.Payload.RE)
	if err != nil {
		return nil, err
	}
	v, err := DecryptAmount(s, sigma, ev.Payload.VTilde)
	if err != nil {
		return nil, err
	}
	if !r.receiveKnown {
		r.vReceive, r.rReceive = big.NewInt(0), big.NewInt(0)
		r.receiveKnown = true
	}
	r.vReceive = new(big.Int).Add(r.vReceive, v)
	r.rReceive = new(big.Int).Add(r.rReceive, TransferBlinding(s, sigma))
	r.rReceive.Mod(r.rReceive, FqModulus)
	return v, nil
}

func (r *replay) apply(ev Event) error {
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
			return nil
		}
		r.vSpend, r.rSpend = big.NewInt(0), big.NewInt(0)
		r.vReceive, r.rReceive = big.NewInt(0), big.NewInt(0)
		r.spendKnown, r.receiveKnown = true, true
		r.entry(ev, "owner", nil, "account registered")

	case "deposit":
		from, to := addr(0), addr(1)
		if to == r.account {
			if ev.AmountPublic == nil {
				return fmt.Errorf("deposit without public amount")
			}
			if !r.receiveKnown {
				r.vReceive, r.rReceive = big.NewInt(0), big.NewInt(0)
				r.receiveKnown = true
			}
			// Deposits commit with zero blinding: C_dep = amount·G.
			r.vReceive = new(big.Int).Add(r.vReceive, ev.AmountPublic)
			r.entry(ev, "recipient", ev.AmountPublic, "public deposit into pending balance")
		} else if from == r.account {
			r.entry(ev, "sender", ev.AmountPublic, "funded a deposit to "+short(to))
		}

	case "merge":
		if addr(0) != r.account {
			return nil
		}
		if r.spendKnown && r.receiveKnown {
			r.vSpend = new(big.Int).Add(r.vSpend, r.vReceive)
			r.rSpend = new(big.Int).Add(r.rSpend, r.rReceive)
			r.rSpend.Mod(r.rSpend, FqModulus)
		} else {
			r.spendKnown = false
			r.warnf("merge at ledger %d folded an unknown accumulator; spendable unknown until the next checkpoint", ev.Ledger)
		}
		r.vReceive, r.rReceive = big.NewInt(0), big.NewInt(0)
		r.receiveKnown = true
		r.entry(ev, "owner", nil, "pending balance folded into spendable")

	case "withdraw":
		from, to := addr(0), addr(1)
		if from != r.account {
			if to == r.account {
				r.entry(ev, "recipient", ev.AmountPublic, "received underlying tokens (outside the confidential balance)")
			}
			return nil
		}
		prev, err := r.checkpoint(ev)
		if err != nil {
			r.spendKnown = false
			r.warnf("withdraw at ledger %d: checkpoint unreadable (%v)", ev.Ledger, err)
		}
		_ = prev
		r.entry(ev, "sender", ev.AmountPublic, "withdrawn to underlying tokens for "+short(to))

	case "transfer":
		from, to := addr(0), addr(1)
		if from == r.account {
			prev, err := r.checkpoint(ev)
			if err != nil {
				r.spendKnown = false
				r.warnf("transfer at ledger %d: checkpoint unreadable (%v)", ev.Ledger, err)
				r.entry(ev, "sender", nil, "sent to "+short(to)+" (checkpoint unreadable)")
			} else {
				var sent *big.Int
				if prev != nil {
					sent = new(big.Int).Sub(prev, r.vSpend)
				}
				r.entry(ev, "sender", sent, "sent to "+short(to))
			}
		}
		if to == r.account {
			v, err := r.credit(ev, sigmaOf(ev))
			if err != nil {
				r.receiveKnown = false
				r.warnf("transfer at ledger %d: inbound amount unreadable (%v)", ev.Ledger, err)
				r.entry(ev, "recipient", nil, "received from "+short(from)+" (unreadable with this key)")
			} else {
				r.entry(ev, "recipient", v, "received from "+short(from))
			}
		}

	case "set_spender":
		account, spender := addr(0), addr(1)
		if account != r.account {
			if spender == r.account {
				r.entry(ev, "spender", nil, "granted an allowance by "+short(account))
			}
			return nil
		}
		prev, err := r.checkpoint(ev)
		if err != nil {
			r.spendKnown = false
			r.warnf("set_spender at ledger %d: checkpoint unreadable (%v)", ev.Ledger, err)
			r.entry(ev, "owner", nil, "escrowed an allowance to "+short(spender))
			return nil
		}
		var escrowed *big.Int
		if prev != nil {
			escrowed = new(big.Int).Sub(prev, r.vSpend)
		}
		note := "escrowed to spender " + short(spender)
		if ev.Payload != nil && ev.Payload.LiveUntilLedger > 0 {
			note += fmt.Sprintf(" (expires ledger %d)", ev.Payload.LiveUntilLedger)
		}
		r.entry(ev, "owner", escrowed, note)

	case "revoke_spender":
		account, spender := addr(0), addr(1)
		if account != r.account {
			if spender == r.account {
				r.entry(ev, "spender", nil, "allowance revoked by "+short(account))
			}
			return nil
		}
		prev, err := r.checkpoint(ev)
		if err != nil {
			r.spendKnown = false
			r.warnf("revoke_spender at ledger %d: checkpoint unreadable (%v)", ev.Ledger, err)
			r.entry(ev, "owner", nil, "reclaimed unspent allowance from "+short(spender))
			return nil
		}
		var reclaimed *big.Int
		if prev != nil {
			reclaimed = new(big.Int).Sub(r.vSpend, prev)
		}
		r.entry(ev, "owner", reclaimed, "reclaimed unspent allowance from "+short(spender))

	case "spender_transfer":
		spender, from, to := addr(0), addr(1), addr(2)
		if to == r.account {
			v, err := r.credit(ev, sigmaAOf(ev))
			if err != nil {
				r.receiveKnown = false
				r.warnf("spender_transfer at ledger %d: inbound amount unreadable (%v)", ev.Ledger, err)
				r.entry(ev, "recipient", nil, "received via spender "+short(spender)+" (unreadable with this key)")
			} else {
				r.entry(ev, "recipient", v, "received from "+short(from)+" via spender "+short(spender))
			}
		}
		if from == r.account && to != r.account {
			// The owner's spendable was already debited at set_spender;
			// the per-transfer amount is encrypted to the recipient and
			// the auditors only — that asymmetry is the protocol, not a
			// missing feature.
			r.entry(ev, "owner", nil,
				"spender "+short(spender)+" paid "+short(to)+" from the escrowed allowance (amount readable by recipient and auditors)")
		}
		if spender == r.account && from != r.account && to != r.account {
			r.entry(ev, "spender", nil, "spent from "+short(from)+"'s allowance to "+short(to))
		}

	default:
		r.entry(ev, "", nil, "unrecognized event kind (ignored)")
	}
	return nil
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
