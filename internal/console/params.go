package console

import "strconv"

// intParam reads a query parameter, falling back rather than erroring.
//
// A malformed page size is a client mistake with an obvious sensible reading;
// rejecting the whole request over it would be pedantry.
func intParam(raw string, fallback int) int {
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

func int64Param(raw string) int64 {
	if raw == "" {
		return 0
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 {
		return 0
	}
	return value
}
