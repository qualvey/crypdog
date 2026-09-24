package scanner

import "time"

func maxScannerHealthAge(interval time.Duration) time.Duration {
	if interval < 10*time.Second {
		interval = 10 * time.Second
	}
	return interval * 3
}
