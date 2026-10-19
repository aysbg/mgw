// Package policy holds the run-scoped authority record and the checks made
// against it: token, expiry, provider, model and the request body cap. It also
// holds registry v2 and the decision table generated from it.
//
// The decision table is ordered candidates, never authorization. A cell says
// which seats to try first for a role, task type, size, risk and strategy. It
// grants nothing. P5 applies these runtime guards to every candidate before a
// request is routed: writer family exclusion, risk requirement, deny list, role
// eligibility, tool grade, budget, and pin match. A seat that is in a cell and
// fails a guard is skipped.
//
// The registry is hashed over canonical JSON (see package parity). It carries
// integers only: prices are micro-USD per 1M tokens, and a float anywhere in the
// payload is rejected.
package policy
