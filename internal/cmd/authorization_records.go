package cmd

import "time"

func authorizationRecordTime(value *time.Time) string {
	if value == nil {
		return "unknown"
	}
	return value.UTC().Format(time.RFC3339)
}
