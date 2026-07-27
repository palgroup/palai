package mcp

import (
	"context"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/client"
)

// sweepOperationTimeout bounds each daemon call so a wedged daemon cannot hang the sweep.
const sweepOperationTimeout = 30 * time.Second

// containerRemover is the sliver of the Docker client the sweep needs — list + remove. The real *client.Client
// satisfies it; a test injects a fake (or drives a real daemon). Keeping it narrow makes the label-scoping
// logic unit-testable and documents that the sweep only ever LISTS and REMOVES.
type containerRemover interface {
	ContainerList(ctx context.Context, options client.ContainerListOptions) (client.ContainerListResult, error)
	ContainerRemove(ctx context.Context, id string, options client.ContainerRemoveOptions) (client.ContainerRemoveResult, error)
}

// Sweeper reclaims orphaned MCP stdio containers — ones a crashed/killed control plane left behind before it
// could teardown a per-call container. It is STRICTLY label-scoped to io.palai.sandbox=mcp, so it NEVER
// touches an engine (io.palai.sandbox=engine) or shell container, and the engine reaper never touches an
// MCP one (§28.13 named gap). A container younger than the grace window is spared (a call may still be
// running); an older one is force-removed. Ceiling: MCP containers are per-call and short-lived, so grace is
// modest — the sweep is the crash-safety net, not the normal teardown path (that is the manager's Kill).
type Sweeper struct {
	client containerRemover
	grace  time.Duration
	now    func() time.Time
	rounds atomic.Int64
	// passFailed reports a failing pass ONCE per process. A sweep failure is almost always a standing
	// condition — no Docker socket, a wedged daemon — not an event, and re-stating it once a minute
	// forever is what a real MCP failure gets buried under. Same shape, same reason, as
	// slack_stream.go's statusUnusable.
	passFailed sync.Once
}

// NewSweeper connects to the daemon described by the standard Docker environment and binds the grace window.
func NewSweeper(grace time.Duration) (*Sweeper, error) {
	apiClient, err := client.New(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("mcp sweep: create docker client: %w", err)
	}
	return NewSweeperWithClient(apiClient, grace), nil
}

// NewSweeperWithClient builds a sweeper over an injected client (tests / a shared client). A non-positive
// grace defaults to two minutes — comfortably past a per-call container's wall-time.
func NewSweeperWithClient(c containerRemover, grace time.Duration) *Sweeper {
	if grace <= 0 {
		grace = 2 * time.Minute
	}
	return &Sweeper{client: c, grace: grace, now: time.Now}
}

// Sweep runs one reconcile pass and returns the number of orphan MCP containers reclaimed. It lists ONLY
// io.palai.sandbox=mcp containers (the daemon-side label filter is the scoping guarantee — an engine
// container is never even returned) and force-removes those older than the grace window.
func (s *Sweeper) Sweep(ctx context.Context) (int, error) {
	listCtx, cancel := context.WithTimeout(ctx, sweepOperationTimeout)
	defer cancel()
	result, err := s.client.ContainerList(listCtx, client.ContainerListOptions{
		All:     true,
		Filters: client.Filters{}.Add("label", sandboxLabel+"="+sandboxLabelMCP),
	})
	if err != nil {
		return 0, fmt.Errorf("mcp sweep: list containers: %w", err)
	}
	cutoff := s.now().Add(-s.grace)
	reclaimed := 0
	var firstErr error
	for _, c := range result.Items {
		// Defence in depth: trust the row's own label, not just the daemon filter.
		if c.Labels[sandboxLabel] != sandboxLabelMCP {
			continue
		}
		if time.Unix(c.Created, 0).After(cutoff) {
			continue // inside the grace window — a call may still be running
		}
		rmCtx, rmCancel := context.WithTimeout(ctx, sweepOperationTimeout)
		_, rmErr := s.client.ContainerRemove(rmCtx, c.ID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
		rmCancel()
		if rmErr != nil && !cerrdefs.IsNotFound(rmErr) {
			if firstErr == nil {
				firstErr = fmt.Errorf("mcp sweep: remove orphan %s: %w", c.ID, rmErr)
			}
			continue // best-effort: the next round retries
		}
		reclaimed++
	}
	return reclaimed, firstErr
}

// Rounds is the number of completed passes (the supervisor liveness counter).
func (s *Sweeper) Rounds() int64 { return s.rounds.Load() }

// Run reconciles every interval until ctx is cancelled (the artifact-orphan-gc supervised loop). A pass
// error is non-fatal — the next tick retries — and every pass advances Rounds().
//
// The error is logged ONCE per process (§3.6 D8). It kept retrying and kept saying so, once a minute,
// which read as an incident and was one only the first time; Rounds() is still the honest liveness
// counter for anything that wants to see the loop is alive. A reclaim is logged every time, because
// that one IS an event.
func (s *Sweeper) Run(ctx context.Context, interval time.Duration) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			reclaimed, err := s.Sweep(ctx)
			s.rounds.Add(1)
			if err != nil {
				s.passFailed.Do(func() {
					log.Printf("mcp orphan-sweep pass failed: %v. The loop keeps retrying; this is logged once per process, "+
						"not once per tick, so a real MCP failure is not buried under it.", err)
				})
			} else if reclaimed > 0 {
				log.Printf("mcp orphan-sweep reclaimed %d orphan container(s)", reclaimed)
			}
		}
	}
}
