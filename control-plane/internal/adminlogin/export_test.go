package adminlogin

import "time"

// SetNow replaces the verifier's clock.
func SetNow(v *Verifier, now func() time.Time) { v.now = now }
