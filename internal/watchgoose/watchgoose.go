package watchgoose

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"
)

const (
	retryDelay = time.Second
	pauseFile  = "/var/run/apiary/watchgoose.pause"
)

func Run(ctx context.Context, endpoint string, interval, timeout time.Duration, nodeID string) {
	client := &http.Client{
		Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	hadFailure := false
	ping := func() {
		if _, err := os.Stat(pauseFile); err == nil {
			return
		} else if !os.IsNotExist(err) {
			log.Printf("managerd: Watchgoose heartbeat for Hive %q paused: cannot inspect pause marker", nodeID)
			return
		}
		if err := sendWithRetry(ctx, client, endpoint); err != nil {
			if ctx.Err() == nil {
				log.Printf("managerd: Watchgoose heartbeat for Hive %q failed: %v", nodeID, err)
				hadFailure = true
			}
			return
		}
		if hadFailure {
			log.Printf("managerd: Watchgoose heartbeat for Hive %q recovered", nodeID)
			hadFailure = false
		}
	}
	ping()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ping()
		}
	}
}

func sendWithRetry(ctx context.Context, client *http.Client, endpoint string) error {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			timer := time.NewTimer(time.Duration(attempt) * retryDelay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return fmt.Errorf("constructing request")
		}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("request failed")
			continue
		}
		resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil
		}
		lastErr = fmt.Errorf("Watchgoose returned HTTP %d", resp.StatusCode)
		if resp.StatusCode < 500 {
			return lastErr
		}
	}
	return lastErr
}
