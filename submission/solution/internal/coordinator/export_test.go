package coordinator

// ExportedProportionalSplitForTest exposes proportionalSplit to the
// black-box coordinator_test package. Compiled only for tests (Go's
// export_test.go convention) — never part of the built binary.
func ExportedProportionalSplitForTest(total int, weights []int64) []int {
	return proportionalSplit(total, weights)
}
