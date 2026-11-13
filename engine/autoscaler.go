package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rs/zerolog/log"

	"go.woodpecker-ci.org/autoscaler/config"
	"go.woodpecker-ci.org/autoscaler/engine/types"
	"go.woodpecker-ci.org/autoscaler/server"
	"go.woodpecker-ci.org/woodpecker/v3/woodpecker-go/woodpecker"
)

// Autoscaler reconciles a pool of woodpecker agents against the current queue
// state. The moving parts are split across the engine package by topic:
//
//   - autoscaler.go (this file): the reconcile loop that ties everything
//     together.
//   - buckets.go / labels.go: how tasks and agents are grouped and matched.
//   - plan.go: how much each bucket should scale up or down.
//   - agents.go: bringing agents up, draining, and removing them.
//   - billing.go: the billing-model-dependent teardown policy.
//   - cleanup.go: reconciling drift between the provider and woodpecker.
type Autoscaler struct {
	client   server.Client
	agents   map[string]*woodpecker.Agent
	config   *config.Config
	provider types.Provider
	// allAgents holds every agent the server returned this reconcile, pool and
	// non-pool alike. The static-capacity netting step reads it to see free
	// slots on agents outside this pool.
	allAgents            []*woodpecker.Agent
	providerCapabilities []types.Capability
	scope                poolScope

	// pendingDeploys remembers, by agent name, the capability each agent was
	// deployed for until it first connects. A freshly deployed agent reports
	// no platform or backend before its first contact, so without this record
	// it would match no bucket and the planner would re-provision the same
	// demand every cycle while startup takes longer than the reconciliation
	// interval. Entries are pruned once the agent connects (its self-reported
	// identity wins from then on) or disappears. The record is in-memory
	// only — after an autoscaler restart, agents still booting are
	// unattributed until they connect, and scale-up in that window is bounded
	// by the MaxAgents footprint as before.
	pendingDeploys map[string]types.Capability

	// danglingSince records, per provider instance name, when it first went
	// missing from the woodpecker agent list. cleanupDanglingAgents defers
	// terminating a missing instance until it has been absent past
	// AgentInactivityTimeout, so a transient woodpecker server restart (which
	// briefly drops every agent's gRPC session at once) does not trigger
	// immediate mass-termination of still-busy agents.
	//
	// The clock is process-local: NewAutoscaler reinitializes it, so an
	// autoscaler restart resets every grace window and grants a genuinely
	// dangling instance another full AgentInactivityTimeout before it is
	// reaped. That errs fail-safe (toward not terminating a possibly-busy
	// agent). AgentInactivityTimeout is intentionally reused as the
	// absence-grace window to stay in parity with cleanupStaleAgents, which
	// anchors its own grace on server-reported agent timestamps.
	danglingSince map[string]time.Time
}

// NewAutoscaler creates a new Autoscaler instance.
// It takes in a Provider, Client and Config, and returns a configured
// Autoscaler struct.
func NewAutoscaler(ctx context.Context, p types.Provider, client server.Client, config *config.Config) (*Autoscaler, error) {
	caps, err := p.Capabilities(ctx)
	if err != nil {
		return nil, fmt.Errorf("could not query provider capabilities: %w", err)
	}
	if err := checkCapabilitiesDisjoint(caps); err != nil {
		return nil, err
	}

	return &Autoscaler{
		provider:             p,
		client:               client,
		config:               config,
		agents:               make(map[string]*woodpecker.Agent),
		pendingDeploys:       make(map[string]types.Capability),
		danglingSince:        make(map[string]time.Time),
		providerCapabilities: caps,
		scope:                instanceAdminPoolScope(),
	}, nil
}

// Reconcile periodically checks the status of the agent pool and adjusts
// it to match the desired capacity based on the current queue state.
//
// The decision is per-bucket: each provider capability merged with the
// configured ExtraAgentLabels is one bucket, and we ask the planner how
// much each bucket needs to scale up or down. Tasks that no bucket can
// serve are excluded from the math — spinning up agents that can't run
// them wouldn't help.
//
// Outdated agents are drained before planning, so a rollout's replacements
// are counted as demand and a retiring agent is never reused as capacity.
func (a *Autoscaler) Reconcile(ctx context.Context) error {
	if err := a.loadAgents(ctx); err != nil {
		return fmt.Errorf("loading agents failed: %w", err)
	}
	if err := a.drainUnmatchedAgents(a.agentBuckets()); err != nil {
		return fmt.Errorf("draining agents with unavailable capabilities failed: %w", err)
	}

	outdated, err := a.outdatedAgentNames(ctx)
	if err != nil {
		return fmt.Errorf("listing outdated agents failed: %w", err)
	}
	if err := a.drainOutdatedAgents(outdated); err != nil {
		return fmt.Errorf("draining outdated agents failed: %w", err)
	}

	queueInfo, err := a.client.QueueInfo()
	if err != nil {
		return fmt.Errorf("loading queue info failed: %w", err)
	}
	log.Debug().
		Int("pending", len(queueInfo.Pending)).
		Int("running", len(queueInfo.Running)).
		Msg("queue snapshot")

	// planScaling already logs the per-bucket plan at debug level — we
	// just dispatch. A bucket that fails to scale (e.g. its capability is out
	// of stock) must neither starve the other buckets nor skip the cleanup
	// below, so its error is only reported once the cycle is through.
	var scaleErr error
	for _, d := range a.planScaling(queueInfo.Pending, queueInfo.Running, outdated) {
		var err error
		switch {
		case d.Delta > 0:
			err = a.createAgents(ctx, d.Bucket, d.Delta, outdated)
		case d.Delta < 0:
			err = a.drainAgents(ctx, d.Bucket, -d.Delta)
		}
		if err != nil {
			scaleErr = errors.Join(scaleErr, fmt.Errorf("scaling bucket %s/%s: %w",
				d.Bucket.Capability.Platform, d.Bucket.Capability.Backend, err))
		}
	}

	// cleanup agents that are only present at the provider or woodpecker
	if err := a.cleanupDanglingAgents(ctx); err != nil {
		return errors.Join(scaleErr, fmt.Errorf("cleaning up dangling agents failed: %w", err))
	}

	// cleanup agents that haven't contacted the server for a while
	if err := a.cleanupStaleAgents(ctx); err != nil {
		return errors.Join(scaleErr, fmt.Errorf("cleaning up stale agents failed: %w", err))
	}

	// remove agents that are drained
	if err := a.removeDrainedAgents(ctx, outdated); err != nil {
		return errors.Join(scaleErr, fmt.Errorf("removing drained agents failed: %w", err))
	}

	return scaleErr
}
