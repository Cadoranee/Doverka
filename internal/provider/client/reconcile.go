package client

import (
	"context"
	"errors"
	"time"

	"github.com/Cadoranee/Doverka/internal/clock"
)

type Resolution string

const (
	ResolutionPresent Resolution = "present"
	ResolutionAbsent  Resolution = "absent"
	ResolutionUnknown Resolution = "unknown"

	VisibilityWindow = 15 * time.Second
	PollInterval     = 1 * time.Second
)

type UnknownPoll struct {
	Window   time.Duration
	Interval time.Duration
	Clock    clock.Clock
	Wait     func(context.Context, time.Duration) error
}

func DefaultUnknownPoll() UnknownPoll {
	return UnknownPoll{Window: VisibilityWindow, Interval: PollInterval}
}

func (c *Client) ResolveUnknown(ctx context.Context, providerID, reference string, acceptedAt time.Time, poll UnknownPoll) (Resolution, Payout, error) {
	if poll.Window <= 0 || poll.Interval <= 0 {
		return ResolutionUnknown, Payout{}, errors.New("invalid poll configuration")
	}

	clk := poll.Clock
	if clk == nil {
		clk = clock.System{}
	}
	wait := poll.Wait
	if wait == nil {
		wait = waitDuration
	}

	deadline := acceptedAt.Add(poll.Window)
	backoff := poll.Interval

	for {
		if err := ctx.Err(); err != nil {
			return ResolutionUnknown, Payout{}, err
		}

		payout, found, findErr := c.FindPayout(ctx, providerID, reference)
		if found {
			return ResolutionPresent, payout, nil
		}
		if err := ctx.Err(); err != nil {
			return ResolutionUnknown, Payout{}, err
		}

		now := clk.Now()
		if !now.Before(deadline) {
			if findErr != nil {
				return ResolutionUnknown, Payout{}, findErr
			}
			return ResolutionAbsent, Payout{}, nil
		}

		delay := pollDelay(backoff, poll.Window, deadline.Sub(now))
		if delay <= 0 {
			return ResolutionUnknown, Payout{}, errors.New("poll delay is exhausted")
		}
		if err := wait(ctx, delay); err != nil {
			return ResolutionUnknown, Payout{}, err
		}
		backoff = nextBackoff(backoff, poll.Window)
	}
}

func pollDelay(backoff, window, remaining time.Duration) time.Duration {
	if remaining <= 0 {
		return 0
	}
	delay := backoff
	if delay > window {
		delay = window
	}
	if delay > remaining {
		return remaining
	}
	return delay
}

func nextBackoff(current, window time.Duration) time.Duration {
	next := current * 2
	if next < current || next > window {
		return window
	}
	return next
}

func waitDuration(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
