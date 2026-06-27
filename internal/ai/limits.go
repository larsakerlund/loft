package ai

import (
	"encoding/json"
	"time"
)

type dayUsage struct {
	tokens int
	day    int64
}

// allowReq is the request rate limit, keyed per (site, user) via a shared token bucket.
func (s *Service) allowReq(site, userID string) bool { return s.reqs.Allow(site + "|" + userID) }

// reserve computes the worst-case token reservation (output cap + an input-token estimate, so an
// omitted upstream usage chunk never under-counts) and reserves it. Returns (reserved, day, ok).
func (s *Service) reserve(site, userID string, body chatBody) (reserved int, day int64, ok bool) {
	inputEst := 0
	if msgs, err := json.Marshal(body.Messages); err == nil {
		inputEst = (len(msgs) + 3) / 4 // ~4 bytes/token, conservative
	}
	reserved = s.cfg.AIMaxTokens + inputEst
	day, ok = s.reserveBudget(site, userID, reserved)
	return reserved, day, ok
}

// reserveBudget reserves `estimate` tokens against BOTH the per-site daily cap and the per-(site,user)
// daily sub-cap, rejecting if either would be exceeded, so one user can't drain the whole site's
// budget. Returns the captured UTC day, which reconcile/refund use so they only adjust the same-day
// bucket (a reservation that spans midnight self-heals rather than corrupting the new day's count).
func (s *Service) reserveBudget(site, userID string, estimate int) (int64, bool) {
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	d := today()
	// Sweep when EITHER map is large; userUsage (keyed site|user) grows faster than the per-site map.
	if len(s.usage) > budgetEvictThreshold || len(s.userUsage) > budgetEvictThreshold {
		s.evictUsageLocked(d)
	}
	su := budgetBucket(s.usage, site, d)
	uu := budgetBucket(s.userUsage, site+"|"+userID, d)
	if su.tokens+estimate > dailyTokensPerSite || uu.tokens+estimate > dailyTokensPerUser {
		return d, false
	}
	su.tokens += estimate
	uu.tokens += estimate
	return d, true
}

// reconcile adjusts a reservation to actual usage (delta may be negative). An abort passes
// actual == reserved, so the reservation stands (no free completions).
func (s *Service) reconcile(site, userID string, day int64, reserved, actual int) {
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	for _, u := range []*dayUsage{s.usage[site], s.userUsage[site+"|"+userID]} {
		if u == nil || u.day != day {
			continue
		}
		u.tokens += actual - reserved
		if u.tokens < 0 {
			u.tokens = 0
		}
	}
}

// refund returns a full reservation, used when the upstream call produced no billable output.
func (s *Service) refund(site, userID string, day int64, reserved int) {
	s.reconcile(site, userID, day, reserved, 0)
}

func (s *Service) evictUsageLocked(d int64) {
	for k, u := range s.usage {
		if u.day != d {
			delete(s.usage, k)
		}
	}
	for k, u := range s.userUsage {
		if u.day != d {
			delete(s.userUsage, k)
		}
	}
}

func budgetBucket(m map[string]*dayUsage, key string, d int64) *dayUsage {
	u := m[key]
	if u == nil || u.day != d {
		u = &dayUsage{day: d}
		m[key] = u
	}
	return u
}

func today() int64 { return time.Now().UnixMilli() / 86_400_000 }
