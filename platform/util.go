package platform

import "time"

// maxBody is the most a request body may hold.
const maxBody = 1 << 20

// dateLayout is how a day is written in the API: 2026-10-28.
const dateLayout = "2006-01-02"

// lagos is Nigerian time (WAT, no daylight saving): days and dates in the API are Nigerian.
var lagos = time.FixedZone("WAT", 3600)

// InvalidError is a request refused for a reason the caller can fix; Message says what.
type InvalidError struct{ Message string }

func (e InvalidError) Error() string { return e.Message }
