package cmd

import "time"

// timeFromUnixUTC is a tiny stdlib wrapper so misc.go can avoid
// reaching into stdlib `time` directly (keeps the import surface
// boring + makes the tparts type obvious).
func timeFromUnixUTC(sec int64) tparts {
	t := time.Unix(sec, 0).Local()
	return tparts{
		Y: t.Year(), M: int(t.Month()), D: t.Day(),
		H: t.Hour(), Min: t.Minute(), S: t.Second(),
	}
}
