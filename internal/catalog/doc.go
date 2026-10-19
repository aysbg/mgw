// Package catalog ingests the live model catalog, merges nothing by itself, and
// carries the curated capability file. It decodes the subset of models.dev the
// gateway needs, converts every price to integer micro-USD per 1M tokens without
// float arithmetic, and writes the pricing snapshot.
//
// Prices are blended at 90% input and 10% output: (9*input + 1*output)/10. The
// ratio changes the ranking and is stated wherever prices are shown.
package catalog
