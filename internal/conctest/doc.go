// Package conctest holds the multi-process concurrency test. It runs the
// built skald binary as many independent processes against one database and
// checks that nothing is lost, duplicated or reordered. The test lives in
// conc_test.go; run it with `make conc`.
package conctest
