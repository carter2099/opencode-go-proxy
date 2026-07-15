package main

import (
	"fmt"
	"sync"
	"time"
)

// picker is the routing core: chooses which account to send a request to,
// applying sticky+hysteresis, tier preference, and highest-balance PAYG.
type picker struct {
	cfg Config

	mu        sync.Mutex
	accounts  []*account
	stickyIdx int   // current sticky-active account (−1 unset)
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
// available (both avoided or all PAYG when disable_payg is set → 503).
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
		return chosen, nil
	}


	// All eligible are PAYG.
	if p.cfg.DisablePayg {
		return nil, fmt.Errorf("zen pay-as-you-go disabled and no go_free accounts available")
	}
	// Pick the account with the highest Zen balance.
	chosen := highestBalance(payg)
	p.stickyIdx = chosen.rrIndex
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

// highestBalance picks the PAYG account with the largest Zen balance. When
// balances are equal or unscraped (both 0), the lower-rrIndex account wins as a
// stable tiebreaker.
func highestBalance(payg []*account) *account {
	best := payg[0]
	best.mu.Lock()
	bestBal := best.payg.BalanceUsd
	best.mu.Unlock()
	for _, a := range payg[1:] {
		a.mu.Lock()
		bal := a.payg.BalanceUsd
		a.mu.Unlock()
		if bal > bestBal {
			bestBal = bal
			best = a
		}
	}
	return best
}