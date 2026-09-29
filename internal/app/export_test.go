package app

import "time"

// SetAlerterClock lets tests control time without sleeping.
func SetAlerterClock(a *Alerter, now func() time.Time) { a.now = now }
