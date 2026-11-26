// lanes.go holds the lanes code.

package webhook

// Lane is the queue a recorded delivery waits in.
type Lane int

const (
	// LaneClient holds every delivery a client reads the result of: pull requests, checks and statuses, pushes.
	LaneClient Lane = iota
	// LaneBulk holds the high-volume Actions deliveries: a CI burst sends several per job.
	LaneBulk
)

// bulkEventTypes are the event types LaneBulk holds.
var bulkEventTypes = []string{"workflow_job", "workflow_run"}

// BulkEventTypes returns the event types LaneBulk holds.
func BulkEventTypes() []string { return append([]string(nil), bulkEventTypes...) }

// LaneOf returns the lane a delivery of eventType waits in.
func LaneOf(eventType string) Lane {
	for _, t := range bulkEventTypes {
		if t == eventType {
			return LaneBulk
		}
	}
	return LaneClient
}

// String names the lane for logs and latency kinds.
func (l Lane) String() string {
	if l == LaneBulk {
		return "bulk"
	}
	return "client"
}

// drainsBulkFirst reports whether a delivery of eventType waits for every bulk delivery recorded before it.
func drainsBulkFirst(eventType string) bool { return eventType == "repository" }

// arrivedBefore orders deliveries the way the journal does.
func arrivedBefore(a, b Stored) bool {
	if !a.ReceivedAt.Equal(b.ReceivedAt) {
		return a.ReceivedAt.Before(b.ReceivedAt)
	}
	return a.DeliveryID < b.DeliveryID
}
