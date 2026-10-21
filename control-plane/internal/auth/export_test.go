package auth

// LimitChallenges lowers the bound on all WebAuthn challenges in memory, so
// tests can fill the store without thousands of calls.
func (s *Service) LimitChallenges(n int) { s.ceremonies.max = n }
