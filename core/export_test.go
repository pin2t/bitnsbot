package core

import "time"

// SetPing shortens the watchdog's interval for a test, before Init starts it.
func SetPing(interval time.Duration) { pingInterval = interval }

// Current is the connection calls go through now, so a test can see a swap.
func Current() any { return current.Load() }
