package repository

// nullEmpty 空字符串在库中存 NULL
func nullEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
