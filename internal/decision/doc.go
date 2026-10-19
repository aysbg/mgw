// Package decision holds the optional typed-decision (systemone) client and
// the gate that decides whether an answer may be used. The gate is the point:
// the client is plumbing. It fills four complexity inputs — type, size, risk
// flags, scout. It never selects a seat, never sees the strategy, and never
// authorises anything.
package decision
