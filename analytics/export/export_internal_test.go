package export

// SetMaxSheetRows lowers the rows per XLSX sheet for a test and returns the restore function.
func SetMaxSheetRows(n int) func() {
	old := maxSheetRows
	maxSheetRows = n
	return func() { maxSheetRows = old }
}
