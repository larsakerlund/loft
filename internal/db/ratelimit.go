package db

// allowWrite is the write rate limit, keyed per (site, user) so one user's write loop can't drain
// the shared budget and 429 their others, mirroring loft.ai's per-(site,user) keying.
func (s *Store) allowWrite(site, userID string) bool { return s.writes.Allow(site + "|" + userID) }
