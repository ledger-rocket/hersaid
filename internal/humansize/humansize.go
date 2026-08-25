// Package humansize renders byte counts for a person to read.
//
// It is a package of its own because two commands print sizes and they must
// print them identically: hear names the size of a file it downloaded, sez the
// size of one it is about to send. render, where this began, links slackread,
// and cmd/sez may not — an import-graph test holds it to that — so the choice
// was a second formatter or a shared one.
package humansize

import "fmt"

// Bytes renders a byte count: whole bytes below a kilobyte and one decimal
// place above it. The steps are 1024 rather than 1000, so 1024 bytes reads as
// 1.0 KB.
func Bytes(n int64) string {
	const step = 1024
	if n < step {
		return fmt.Sprintf("%d B", n)
	}
	value, unit := float64(n)/step, ""
	for _, u := range []string{"KB", "MB", "GB", "TB"} {
		unit = u
		if value < step {
			break
		}
		value /= step
	}
	return fmt.Sprintf("%.1f %s", value, unit)
}
