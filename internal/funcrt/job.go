package funcrt

import (
	"context"
	"fmt"
	"time"
)

// ScheduledEvent returns an EventBridge Scheduled Event payload.
func ScheduledEvent(now time.Time, id string) []byte {
	return fmt.Appendf(nil,
		`{"version":"0","id":%q,"detail-type":"Scheduled Event","source":"aws.events","account":"000000000000","time":%q,"region":"local","resources":[],"detail":{}}`,
		id, now.UTC().Format(time.RFC3339))
}

// RunJob waits up to the pool's timeout for a ready bootstrap, invokes it once
// with payload and returns nil only on a 200 result.
func RunJob(ctx context.Context, p *Pool, payload []byte) error {
	deadline := time.NewTimer(p.cfg.Timeout)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for !p.Ready() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("bootstrap not ready within %s", p.cfg.Timeout)
		case <-tick.C:
		}
	}
	res := p.Invoke(ctx, payload)
	if res.Status != 200 {
		return fmt.Errorf("invocation failed with status %d: %s", res.Status, res.Payload)
	}
	return nil
}
