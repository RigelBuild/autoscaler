package engine

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"time"

	"github.com/rs/zerolog/log"

	"go.woodpecker-ci.org/autoscaler/config"
	"go.woodpecker-ci.org/autoscaler/engine/labelfilter"
	"go.woodpecker-ci.org/autoscaler/engine/types"
	"go.woodpecker-ci.org/autoscaler/server"
	"go.woodpecker-ci.org/autoscaler/utils"
	"go.woodpecker-ci.org/woodpecker/v3/woodpecker-go/woodpecker"
)

// agentListPageSize is the server's maxPageSize for /api/agents
// (server/router/middleware/session/pagination.go). woodpecker-go's AgentList()
// fetches only the first page; loadAgents warns if the fleet reaches this size,
// since the netting step would then under-count non-pool capacity.
const agentListPageSize = 50

type Autoscaler struct {
	client server.Client
	// agents holds only this pool's agents (name matches the PoolID regex),
	// populated by loadAgents.
	agents []*woodpecker.Agent
	// allAgents holds every agent the server returned this reconcile, used by
	// the shared-demand netting step to see free capacity on non-pool (static)
	// agents. Also populated by loadAgents.
	allAgents []*woodpecker.Agent
	config    *config.Config
	provider  types.Provider
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
func NewAutoscaler(p types.Provider, client server.Client, config *config.Config) Autoscaler {
	return Autoscaler{
		provider:      p,
		client:        client,
		config:        config,
		danglingSince: make(map[string]time.Time),
	}
}

// inTeardownWindow reports whether the agent is currently within the teardown
// window before one of its paid-hour boundaries (anchored at its creation
// time). Agents that have not reported a creation time are never in the window.
func (a *Autoscaler) inTeardownWindow(agent *woodpecker.Agent) bool {
	if agent.Created == 0 {
		return false
	}

	age := time.Since(time.Unix(agent.Created, 0))
	if age < 0 {
		return false
	}

	window := a.config.AgentBillingTeardownMargin + a.config.ReconciliationInterval
	// A window covering a whole hour (or more) means every moment qualifies.
	if window >= time.Hour {
		return true
	}

	return age%time.Hour >= time.Hour-window
}

func (a *Autoscaler) loadAgents(_ context.Context) error {
	a.agents = []*woodpecker.Agent{}

	agents, err := a.client.AgentList()
	if err != nil {
		return fmt.Errorf("client.AgentList: %w", err)
	}

	// The server paginates /api/agents at maxPageSize=50 and the woodpecker-go
	// AgentList() fetches only the first page. The shared-demand netting in
	// calcAgents needs to see free capacity on non-pool agents, so a truncated
	// list would under-count static capacity and over-provision the pool
	// (bounded by MaxAgents, so it errs to burst-relief, never to starvation).
	// The real fleet is a handful of agents; warn loudly if it ever approaches
	// the page limit so this is caught before it silently degrades.
	if len(agents) >= agentListPageSize {
		log.Warn().Int("agents", len(agents)).Int("page_size", agentListPageSize).
			Msg("agent list hit the server page size; shared-demand netting may under-count non-pool capacity and over-provision (bounded by MaxAgents)")
	}
	a.allAgents = agents

	r, err := regexp.Compile(fmt.Sprintf("pool-%s-agent-.*?", a.config.PoolID))
	if err != nil {
		return fmt.Errorf("could not create regex matcher for agent names by pool ID: %w", err)
	}

	for _, agent := range agents {
		if r.MatchString(agent.Name) {
			a.agents = append(a.agents, agent)
		}
	}

	return nil
}

func (a *Autoscaler) getPoolAgents(excludeNoSchedule bool) []*woodpecker.Agent {
	agents := make([]*woodpecker.Agent, 0)
	for _, agent := range a.agents {
		if excludeNoSchedule && agent.NoSchedule {
			continue
		}
		agents = append(agents, agent)
	}
	return agents
}

func (a *Autoscaler) createAgents(ctx context.Context, amount int) error {
	suffixLength := 4

	reactivatedAgents := 0

	// try to re-activate agents that are in no-schedule state
	for i := 0; i < amount; i++ {
		for _, agent := range a.agents {
			if agent.NoSchedule {
				log.Info().Str("agent", agent.Name).Msg("reactivate agent")
				agent.NoSchedule = false
				_, err := a.client.AgentUpdate(agent)
				if err != nil {
					return fmt.Errorf("client.AgentUpdate: %w", err)
				}
				reactivatedAgents++
			}
		}
	}

	// create new agents
	for i := 0; i < amount-reactivatedAgents; i++ {
		agent, err := a.client.AgentCreate(&woodpecker.Agent{
			Name: fmt.Sprintf("pool-%s-agent-%s", a.config.PoolID, utils.RandomString(suffixLength)),
		})
		if err != nil {
			return fmt.Errorf("client.AgentCreate: %w", err)
		}

		log.Info().Str("agent", agent.Name).Msg("deploying agent")

		err = a.provider.DeployAgent(ctx, agent)
		if err != nil {
			return fmt.Errorf("types.DeployAgent: %w", err)
		}

		a.agents = append(a.agents, agent)
	}

	return nil
}

func (a *Autoscaler) drainAgents(_ context.Context, amount int) error {
	for i := 0; i < amount; i++ {
		for _, agent := range a.agents {
			// agent is already marked for draining
			if agent.NoSchedule {
				continue
			}

			// agent has never contacted the server => not ready for draining
			if agent.LastContact == 0 {
				continue
			}

			if a.config.BillingModel == types.BillingHourlyRoundUp {
				// hourly-round-up: the hour is already paid for, so keep the
				// agent schedulable until just before its hour boundary even
				// while idle, then drain it inside the teardown window.
				if !a.inTeardownWindow(agent) {
					continue
				}
			} else if time.Since(time.Unix(agent.LastWork, 0)) < a.config.AgentIdleTimeout {
				// agent has recently done work => not ready for draining
				continue
			}

			log.Info().Str("agent", agent.Name).Msg("drain agent")
			agent.NoSchedule = true
			_, err := a.client.AgentUpdate(agent)
			if err != nil {
				return fmt.Errorf("client.AgentUpdate: %w", err)
			}
			break
		}
	}

	return nil
}

func (a *Autoscaler) isAgentIdle(agent *woodpecker.Agent) (bool, error) {
	tasks, err := a.client.AgentTasksList(agent.ID)
	if err != nil {
		return false, fmt.Errorf("client.AgentTasksList: %w", err)
	}

	// agent still has tasks => not idle
	if len(tasks) > 0 {
		return false, nil
	}

	// hourly-round-up: recency of work does not gate removal. The paid hour is
	// kept warm by the drain stage; once an agent is eligible for removal the
	// only thing that protects it is an in-flight task (checked above).
	if a.config.BillingModel == types.BillingHourlyRoundUp {
		return true, nil
	}

	// agent has done work recently => not idle
	if time.Since(time.Unix(agent.LastWork, 0)) < a.config.AgentIdleTimeout {
		return false, nil
	}

	return true, nil
}

func (a *Autoscaler) removeAgent(ctx context.Context, agent *woodpecker.Agent, reason string) error {
	isIdle, err := a.isAgentIdle(agent)
	if err != nil {
		return err
	}
	if !isIdle {
		log.Info().Str("agent", agent.Name).Msg("agent is still processing workload")
		return nil
	}

	log.Info().Str("agent", agent.Name).Str("reason", reason).Msgf("removing agent")

	err = a.provider.RemoveAgent(ctx, agent)
	if err != nil {
		return err
	}

	err = a.client.AgentDelete(agent.ID)
	if err != nil {
		return fmt.Errorf("client.AgentDelete: %w", err)
	}

	filteredAgents := make([]*woodpecker.Agent, 0)
	for _, a := range a.agents {
		if a.ID != agent.ID {
			filteredAgents = append(filteredAgents, a)
		}
	}
	a.agents = filteredAgents

	return nil
}

func (a *Autoscaler) removeDrainedAgents(ctx context.Context) error {
	for _, agent := range a.getPoolAgents(false) {
		if !agent.NoSchedule {
			continue
		}

		// hourly-round-up: a drained agent that rolled into a fresh paid hour
		// (e.g. it was busy at the boundary) stays up until its next teardown
		// window rather than wasting the hour just bought.
		if a.config.BillingModel == types.BillingHourlyRoundUp && !a.inTeardownWindow(agent) {
			continue
		}

		err := a.removeAgent(ctx, agent, "was drained")
		if err != nil {
			return err
		}
	}

	return nil
}

func (a *Autoscaler) cleanupDanglingAgents(ctx context.Context) error {
	woodpeckerAgents := a.getPoolAgents(false)
	providerAgentNames, err := a.provider.ListDeployedAgentNames(ctx)
	if err != nil {
		return err
	}

	// Snapshot the provider's current instance names so danglingSince can be
	// garbage-collected below. A name enters danglingSince while it is on the
	// provider but missing from woodpecker; if the provider instance then
	// disappears entirely (e.g. an out-of-band Spot reclaim) before its grace
	// window elapses, the loop below never visits it again — so without this
	// its entry would leak forever. Names are randomized per create, so the
	// leak is unbounded over the process lifetime, not bounded by pool size
	// (transient-restart grace).
	currentProviderNames := make(map[string]struct{}, len(providerAgentNames))
	for _, name := range providerAgentNames {
		currentProviderNames[name] = struct{}{}
	}

	// remove agents that are not in the woodpecker agent list anymore
	for _, agentName := range providerAgentNames {
		found := false
		for _, agent := range woodpeckerAgents {
			if agent.Name == agentName {
				found = true
				break
			}
		}

		if found {
			// Instance is healthy / reappeared: reset any grace clock.
			delete(a.danglingSince, agentName)
			continue
		}

		// Instance is missing from the woodpecker agent list. Apply grace
		// parity with cleanupStaleAgents: don't terminate on the first missing
		// pass, so a transient woodpecker server restart (which briefly drops
		// every agent's gRPC session at once) is tolerated.
		since := a.danglingSince[agentName]
		if since.IsZero() {
			a.danglingSince[agentName] = time.Now()
			log.Info().Str("agent", agentName).Str("reason", "not found on woodpecker").Msg("deferring agent removal pending grace window")
			continue
		}

		if time.Since(since) <= a.config.AgentInactivityTimeout {
			continue
		}

		log.Info().Str("agent", agentName).Str("reason", "not found on woodpecker").Msg("remove agent")
		if err := a.provider.RemoveAgent(ctx, &woodpecker.Agent{Name: agentName}); err != nil {
			return fmt.Errorf("types.RemoveAgent: %w", err)
		}
		delete(a.danglingSince, agentName)

		// remove agent from providerAgentNames
		_providerAgentNames := make([]string, 0)
		for _, name := range providerAgentNames {
			if name != agentName {
				_providerAgentNames = append(_providerAgentNames, name)
			}
		}
		providerAgentNames = _providerAgentNames
	}

	// Garbage-collect grace clocks for instances the provider no longer lists
	// at all (terminated out-of-band mid-grace): the loop above only clears
	// entries on the reappear or past-grace-terminate paths, so an instance
	// that vanishes from the provider before its window elapses would leak.
	for name := range a.danglingSince {
		if _, ok := currentProviderNames[name]; !ok {
			delete(a.danglingSince, name)
		}
	}

	// remove agents that do not exist on the provider anymore
	for _, agent := range woodpeckerAgents {
		found := false
		for _, agentName := range providerAgentNames {
			if agent.Name == agentName {
				found = true
				break
			}
		}

		if !found {
			log.Info().Str("agent", agent.Name).Str("reason", "not found on provider").Msg("remove agent")
			if err = a.client.AgentDelete(agent.ID); err != nil {
				return fmt.Errorf("client.AgentDelete: %w", err)
			}

			// remove agent from woodpeckerAgents
			_woodpeckerAgents := make([]*woodpecker.Agent, 0)
			for _, a := range a.agents {
				if a.Name != agent.Name {
					woodpeckerAgents = append(woodpeckerAgents, a)
				}
			}
			a.agents = _woodpeckerAgents
		}
	}

	return nil
}

func (a *Autoscaler) cleanupStaleAgents(ctx context.Context) error {
	// remove agents that haven't contacted the server for a while (including agents that never contacted the server)
	for _, agent := range a.getPoolAgents(false) {
		if agent.NoSchedule {
			continue
		}

		lastContact := agent.LastContact

		// if agent has never contacted the server, use the creation time
		if lastContact == 0 {
			lastContact = agent.Created
		}

		if time.Since(time.Unix(lastContact, 0)) > a.config.AgentInactivityTimeout {
			err := a.removeAgent(ctx, agent, "hasn't connected to the server for a while")
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func (a *Autoscaler) getQueueInfo(_ context.Context) (*woodpecker.Info, error) {
	queueInfo, err := a.client.QueueInfo()
	if err != nil {
		return nil, fmt.Errorf("client.QueueInfo: %w", err)
	}
	return queueInfo, nil
}

// poolAgentIDs returns the set of agent IDs belonging to this pool. When
// excludeNoSchedule is true, draining (NoSchedule) agents are omitted, keeping
// the set symmetric with getPoolAgents(true) (the available-capacity offset).
// When false, every pool agent is included — nonPoolFreeSlots uses that to
// exclude the whole pool from "other/static" capacity, so a draining pool agent
// is never miscounted as static capacity.
func (a *Autoscaler) poolAgentIDs(excludeNoSchedule bool) map[int64]struct{} {
	ids := make(map[int64]struct{}, len(a.agents))
	for _, agent := range a.agents {
		if excludeNoSchedule && agent.NoSchedule {
			continue
		}
		ids[agent.ID] = struct{}{}
	}
	return ids
}

// countPoolRunning counts running tasks assigned to this pool's schedulable
// agents, using the exact AgentID→pool assignment rather than label inference.
// Draining (NoSchedule) pool agents are excluded so an in-flight last task is
// counted as neither demand (here) nor capacity (availablePoolAgents excludes
// them too). Otherwise a draining agent finishing its last task inflates
// required by 1, and createAgents reactivates the very agent being drained —
// defeating scale-down for the whole drain window.
func (a *Autoscaler) countPoolRunning(running []woodpecker.Task) int {
	poolIDs := a.poolAgentIDs(true)
	n := 0
	for _, task := range running {
		if _, ok := poolIDs[task.AgentID]; ok {
			n++
		}
	}
	return n
}

// freeStaticSlots models the currently-free capacity on each non-pool agent as
// a per-agent filter + remaining-slot count. Slots are Agent.Capacity minus the
// running tasks already assigned to that agent. Pool agents are excluded — the
// netting step only credits demand that capacity *outside* this pool can absorb.
type freeStaticSlots struct {
	filter labelfilter.Filter
	free   int
}

func (a *Autoscaler) nonPoolFreeSlots(running []woodpecker.Task) []*freeStaticSlots {
	// false: a draining pool agent is still a pool agent — exclude the whole
	// pool from "other/static" capacity, never count it as a static slot.
	poolIDs := a.poolAgentIDs(false)

	assigned := make(map[int64]int)
	for _, task := range running {
		assigned[task.AgentID]++
	}

	slots := make([]*freeStaticSlots, 0, len(a.allAgents))
	for _, agent := range a.allAgents {
		if _, ok := poolIDs[agent.ID]; ok {
			continue // pool agent — not "other" capacity
		}
		if agent.NoSchedule {
			continue // draining/quarantined — cannot take new work
		}
		// A cleanly-offline agent's server row persists (Capacity>0, zero running,
		// NoSchedule=false) and would otherwise credit phantom free slots that net
		// out real pending demand — so the pool never scales to cover a dead
		// static agent. Skip an agent whose last contact is stale, mirroring the
		// liveness window cleanupStaleAgents already enforces.
		lastContact := agent.LastContact
		if lastContact == 0 {
			lastContact = agent.Created
		}
		if time.Since(time.Unix(lastContact, 0)) > a.config.AgentInactivityTimeout {
			continue
		}
		free := int(agent.Capacity) - assigned[agent.ID]
		if free <= 0 {
			continue
		}
		slots = append(slots, &freeStaticSlots{filter: labelfilter.AgentFilter(agent), free: free})
	}
	return slots
}

func (a *Autoscaler) calcAgents(ctx context.Context) (float64, error) {
	queueInfo, err := a.getQueueInfo(ctx)
	if err != nil {
		return 0, err
	}

	poolFilter := labelfilter.NewPoolFilter(a.config.ExtraAgentLabels)
	staticSlots := a.nonPoolFreeSlots(queueInfo.Running)

	eligiblePending := 0
	nettedOut := 0
	for _, task := range queueInfo.Pending {
		if !poolFilter.Satisfiable(task) {
			continue // pool can't run it — the label-blind waste this fixes
		}
		// Net out a free non-pool slot that can also run this shared-eligible
		// task (greedy, first fit, in queue order).
		//
		// This is a first-fit approximation of maximum bipartite matching
		// (tasks ↔ static slots). It can under-net when an early task greedily
		// takes a broad-filter slot that a later, more-constrained task also
		// needed — over-provisioning the pool by up to the number of such
		// order-dependent collisions. The error is always in the safe
		// direction (extra burst capacity, never starvation), bounded by
		// MaxAgents, and self-corrects on the next reconcile. Static agents are
		// few and mostly homogeneous, so exact matching (most-constrained-first
		// ordering, or Hopcroft–Karp) is not worth the complexity here.
		absorbed := false
		for _, slot := range staticSlots {
			if slot.free > 0 && slot.filter.Satisfiable(task) {
				slot.free--
				nettedOut++
				absorbed = true
				break
			}
		}
		if !absorbed {
			eligiblePending++
		}
	}

	poolRunning := a.countPoolRunning(queueInfo.Running)

	log.Debug().Msgf("queue info (pool %s): eligiblePending = %d netted = %d poolRunning = %d",
		a.config.PoolID, eligiblePending, nettedOut, poolRunning)

	required := math.Ceil(float64(eligiblePending+poolRunning) / float64(a.config.WorkflowsPerAgent))

	availablePoolAgents := len(a.getPoolAgents(true))
	maxUp := float64(a.config.MaxAgents - availablePoolAgents)
	maxDown := float64(availablePoolAgents - a.config.MinAgents)

	reqPoolAgents := required - float64(availablePoolAgents)
	reqPoolAgents = math.Max(reqPoolAgents, -maxDown)
	reqPoolAgents = math.Min(reqPoolAgents, maxUp)

	log.Debug().Msgf("capacity info (pool %s): required = %v pool = %v/%v limits = %v/%v",
		a.config.PoolID, required, availablePoolAgents, reqPoolAgents, maxUp, maxDown)

	return reqPoolAgents, nil
}

// Reconcile periodically checks the status of the agent pool and adjusts it to match
// the desired capacity based on the current queue state.
func (a *Autoscaler) Reconcile(ctx context.Context) error {
	if err := a.loadAgents(ctx); err != nil {
		return fmt.Errorf("loading agents failed: %w", err)
	}

	reqPoolAgents, err := a.calcAgents(ctx)
	if err != nil {
		return fmt.Errorf("calculating agents failed: %w", err)
	}

	if reqPoolAgents > 0 {
		num := int(math.Abs(reqPoolAgents))
		log.Debug().Msgf("starting %d additional agents", num)

		if err := a.createAgents(ctx, num); err != nil {
			return fmt.Errorf("creating agents failed: %w", err)
		}
	}

	if reqPoolAgents < 0 {
		num := int(math.Abs(reqPoolAgents))

		log.Debug().Msgf("checking %d agents if ready for draining", num)
		if err := a.drainAgents(ctx, num); err != nil {
			return fmt.Errorf("draining agents failed: %w", err)
		}
	}

	// cleanup agents that are only present at the provider or woodpecker
	if err := a.cleanupDanglingAgents(ctx); err != nil {
		return fmt.Errorf("cleaning up dangling agents failed: %w", err)
	}

	// cleanup agents that haven't contacted the server for a while
	if err := a.cleanupStaleAgents(ctx); err != nil {
		return fmt.Errorf("cleaning up stale agents failed: %w", err)
	}

	// remove agents that are drained
	if err := a.removeDrainedAgents(ctx); err != nil {
		return fmt.Errorf("removing drained agents failed: %w", err)
	}

	return nil
}
