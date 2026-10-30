package platform

import "time"

// Alerter tells the owner that something needs a person. Alert returns at once and a failure to
// deliver is only logged; key names the problem and the same key is not sent again within throttle.
// The bills package's email alerts implement it.
type Alerter interface {
	Alert(key string, throttle time.Duration, subject, body string)
}
