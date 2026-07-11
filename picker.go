package main

import (
	"fmt"
	"sync"
	"time"
)

// picker is the routing core: chooses which account to send a request to,
// applying sticky+hysteresis, tier preference, and PAYG round-robin.
type picker struct {
	cfg Config

	mu        sync.Mutex
	accounts  []*account
	stickyIdx int   // current sticky-active account (−1 unset)
	rrCursor  int   // round-robin cursor across PAYG-eligible accounts
	paygRR    bool  // last choice was PAYG round-robin; toggles alternation
}

func newPicker(cfg Config) *picker {
	p := &picker{cfg: cfg, stickyIdx: -1}
	for i := range cfg.Accounts {
		p.accounts = append(p.accounts, &account{
			cfg:     cfg.Accounts[i],
			tier:    tierGoFree, // optimistic until scrape/cost says otherwise
			rrIndex: i,
		})
	}
	return p
}

// choose returns the account to handle this request, or an error if none are
// available (both avoided → 503).
func (p *picker) choose(now time.Time) (*account, error) {
	eligible := make([]*account, 0, len(p.accounts))
	for _, a := range p.accounts {
		if !a.isAvoided(now) {
			eligible = append(eligible, a)
		}
	}
	if len(eligible) == 0 {
		return nil, fmt.Errorf("no available accounts (all in 401 cooldown)")
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// Split by tier.
	var free, payg []*account
	for _, a := range eligible {
		a.mu.Lock()
		t := a.tier
		a.mu.Unlock()
		switch t {
		case tierGoFree:
			free = append(free, a)
		default:
			payg = append(payg, a)
		}
	}

	if len(free) > 0 {
		chosen := pickByLoadHysteresis(p, free, now)
		p.stickyIdx = chosen.rrIndex
		p.paygRR = false
		return chosen, nil
	}

	// All eligible are PAYG → round-robin across them to spread the $25 caps.
	chosen := roundRobin(p, payg, now)
	p.stickyIdx = chosen.rrIndex
	p.paygRR = true
	return chosen, nil
}

// pickByLoadHysteresis keeps the sticky account unless another free account's
// load is at least hysteresisPoints lower.
func pickByLoadHysteresis(p *picker, free []*account, now time.Time) *account {
	lowest := free[0]
	for _, a := range free[1:] {
		if a.load() < lowest.load() {
			lowest = a
		}
	}
	if p.stickyIdx >= 0 {
		for _, a := range free {
			if a.rrIndex == p.stickyIdx {
				if lowest.load() <= a.load()-p.cfg.HysteresisPoints {
					return lowest
				}
				return a // keep sticky
			}
		}
	}
	return lowest
}

// roundRobin alternates accounts across calls. For two accounts this simply
// toggles; the cursor generalises to N.
func roundRobin(p *picker, payg []*account, now time.Time) *account {
	// Sort by rrIndex for a stable cursor.
	byRR := make([]*account, 0, len(payg))
	byRR = append(byRR, payg...)
	for i := 0; i < len(byRR); i++ {
		for j := i + 1; j < len(byRR); j++ {
			if byRR[j].rrIndex < byRR[i].rrIndex {
				byRR[i], byRR[j] = byRR[j], byRR[i]
			}
		}
	}
	p.rrCursor %= len(byRR)
	chosen := byRR[p.rrCursor]
	p.rrCursor = (p.rrCursor + 1) % len(byRR)
	return chosen
}