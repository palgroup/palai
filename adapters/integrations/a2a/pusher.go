package a2a

// STUB (RED stage, E19 T4): the naive push delivery — marshal, POST once, hope. It exists only to turn the
// pusher suite's guarantees into REAL assertion failures instead of compile errors. Every D11/D12 guarantee
// is deliberately absent here and lands in the next commit.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/palgroup/palai/adapters/integrations/webhook"
)

const PushTokenHeader = "X-A2A-Notification-Token"

var ErrPushRefused = errors.New("a2a: push notification refused by policy")

type PushPolicy struct {
	AllowedHosts []string
	AllowPrivate bool
	TimeoutMS    int
	MaxAttempts  int
	RetryWindow  time.Duration
	BaseBackoff  time.Duration
	MaxBackoff   time.Duration
}

type PushFailure struct {
	ConfigID       string
	NotificationID string
	Attempts       int
	StatusCode     int
	Err            error
}

func (f PushFailure) String() string {
	return fmt.Sprintf("push %s (config %s) failed after %d attempts: status=%d err=%v",
		f.NotificationID, f.ConfigID, f.Attempts, f.StatusCode, f.Err)
}

type PushStats struct {
	Delivered int64
	Dead      int64
	Refused   int64
}

type WebhookPusher struct {
	Policy     PushPolicy
	DeadLetter func(context.Context, PushFailure)

	sender    *webhook.Sender
	wg        sync.WaitGroup
	delivered atomic.Int64
	dead      atomic.Int64
	refused   atomic.Int64
}

func NewWebhookPusher(sender *webhook.Sender, policy PushPolicy) *WebhookPusher {
	return &WebhookPusher{Policy: policy, sender: sender}
}

func (p *WebhookPusher) Push(ctx context.Context, cfg PushNotificationConfig, resp StreamResponse) error {
	body, err := json.Marshal(resp.Task) // STUB: the opaque-payload bug — a bare Task, not a StreamResponse
	if err != nil {
		return err
	}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		res := p.sender.Deliver(ctx, webhook.Destination{
			URL: cfg.URL, AllowPrivate: p.Policy.AllowPrivate, TimeoutMS: p.Policy.TimeoutMS,
		}, body)
		if res.StatusCode >= 200 && res.StatusCode < 300 {
			p.delivered.Add(1)
		}
	}()
	return nil
}

func (p *WebhookPusher) Wait() { p.wg.Wait() }

func (p *WebhookPusher) Stats() PushStats {
	return PushStats{Delivered: p.delivered.Load(), Dead: p.dead.Load(), Refused: p.refused.Load()}
}
