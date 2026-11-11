package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/rs/zerolog/log"

	"go.woodpecker-ci.org/woodpecker/v3/woodpecker-go/woodpecker"
)

// This file reconciles drift between the two sources of truth for pool agents:
// the provider (what is actually deployed) and the woodpecker server (what is
// registered). Agents that exist on only one side, or that have gone silent,
// are cleaned up here. This runs independently of the scale up/down decision.

func (a *Autoscaler) cleanupDanglingAgents(ctx context.Context) error {
	providerAgentNames, err := a.provider.ListDeployedAgentNames(ctx)
	if err != nil {
		return err
	}

	// Build the provider-side set up front so the two reconciliation
	// directions below are independent of each other.
	providerSet := make(map[string]struct{}, len(providerAgentNames))
	for _, name := range providerAgentNames {
		providerSet[name] = struct{}{}
	}

	// On provider but not on woodpecker → tear down on the provider, but only
	// once the instance has been absent past the grace window. A transient
	// woodpecker server restart drops every agent's session at once, so the
	// first missing pass must not terminate a machine that may still be busy.
	for name := range providerSet {
		if _, ok := a.agents[name]; ok {
			// Instance is healthy / reappeared: reset any grace clock.
			delete(a.danglingSince, name)
			continue
		}

		since, ok := a.danglingSince[name]
		if !ok {
			a.danglingSince[name] = time.Now()
			log.Info().Str("agent", name).Str("reason", "not found on woodpecker").Msg("deferring agent removal pending grace window")
			continue
		}
		if time.Since(since) <= a.config.AgentInactivityTimeout {
			continue
		}

		log.Info().Str("agent", name).Str("reason", "not found on woodpecker").Msg("remove agent")
		if err := a.provider.RemoveAgent(ctx, &woodpecker.Agent{Name: name}); err != nil {
			return fmt.Errorf("types.RemoveAgent: %w", err)
		}
		delete(a.danglingSince, name)
	}

	// Garbage-collect grace clocks for instances the provider no longer lists
	// at all (terminated out-of-band mid-grace): the loop above only clears
	// entries on the reappear or past-grace-terminate paths, so an instance
	// that vanishes from the provider before its window elapses would leak.
	for name := range a.danglingSince {
		if _, ok := providerSet[name]; !ok {
			delete(a.danglingSince, name)
		}
	}

	// On woodpecker but not on provider → delete on woodpecker.
	for name, agent := range a.agents {
		if _, ok := providerSet[name]; ok {
			continue
		}
		log.Info().Str("agent", name).Str("reason", "not found on provider").Msg("remove agent")
		if err := a.client.AgentDelete(agent.ID); err != nil {
			return fmt.Errorf("client.AgentDelete: %w", err)
		}
		delete(a.agents, name)
	}

	return nil
}

func (a *Autoscaler) cleanupStaleAgents(ctx context.Context) error {
	// remove agents that haven't contacted the server for a while (including agents that never contacted the server)
	for _, agent := range a.agents {
		if agent.NoSchedule {
			continue
		}

		lastContact := agent.LastContact
		timeout := a.config.AgentInactivityTimeout

		// An agent that has never contacted the server is judged from its
		// creation time, against the boot deadline when one is configured.
		if lastContact == 0 {
			lastContact = agent.Created
			if a.config.AgentCreationTimeout > 0 {
				timeout = a.config.AgentCreationTimeout
			}
		}

		if time.Since(time.Unix(lastContact, 0)) > timeout {
			err := a.removeAgent(ctx, agent, false, "hasn't connected to the server for a while")
			if err != nil {
				return err
			}
		}
	}

	return nil
}
