// Package ledger is the cross-process token ledger: reserve before an upstream
// call, settle after it. State lives in one JSON file guarded by an flock on a
// sibling lock file and replaced by atomic rename, so any number of gateway
// processes can share one ledger without overspending.
package ledger
