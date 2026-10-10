// Copyright 2026 Woodpecker Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package labelfilter models, per pool, which queued tasks a pool's agents can
// actually run. It mirrors the Woodpecker server scheduler's own matching rule
// (server/scheduler/filter.go) exactly, so the autoscaler counts only
// label-satisfiable demand instead of the label-blind fleet-wide Stats.Pending.
//
// The match logic (matchFilter / requiredLabelsMissing) is a verbatim
// transliteration of createFilterFunc / requiredLabelsMissing; it is defended
// by a test table transliterated from the scheduler's own filter_test.go. Any
// divergence there silently over- or under-scales the pool.
package labelfilter

import (
	"maps"
	"strconv"
	"strings"

	"go.woodpecker-ci.org/woodpecker/v3/pipeline"
	"go.woodpecker-ci.org/woodpecker/v3/woodpecker-go/woodpecker"
)

// idNotSet mirrors server/model.IDNotSet (-1): an agent's OrgID is "unset" when
// it equals this sentinel, which the server treats as a global (org-id="*")
// agent. Defined locally rather than importing server/model — that package
// carries DB (xorm), crypto, and cron dependencies the autoscaler otherwise
// never needs, and this is a single stable sentinel.
const idNotSet int64 = -1

// synthesizedLabels counts the defaults each filter may add: repo, platform, org-id.
const synthesizedLabels = 3

// transliteratedFromVersion is the exact go.woodpecker-ci.org/woodpecker/v3
// module version whose server/scheduler/filter.go the match logic below was
// transliterated from. The parity is a manual invariant: matchFilter /
// requiredLabelsMissing mirror the scheduler's unexported createFilterFunc /
// requiredLabelsMissing, which cannot be imported for a live diff. The
// TestParityVersionPin tripwire fails when the pinned module version drifts
// from this const, forcing a re-read of the upstream filter (and a bump of
// this const) on every dependency update — see labelfilter_test.go.
const transliteratedFromVersion = "v3.16.0"

// Filter is the effective label set an agent advertises to the scheduler: its
// configured/custom labels plus the server/agent-stamped defaults (repo="*",
// org-id="*") that apply to every agent. A task is runnable on the agent iff
// Satisfiable reports true. Built by NewPoolFilter (for the managed elastic
// pool) or AgentFilter (for an existing static agent).
type Filter struct {
	labels map[string]string
}

// NewPoolFilter builds the modeled filter for the elastic pool this autoscaler
// manages, from the pool's WOODPECKER_AGENT_LABELS (config.ExtraAgentLabels)
// and the platform its agents will self-report (config.PoolPlatform).
//
// It synthesizes the same defaults the real agents get so server-stamped task
// labels don't make every task unmatchable:
//   - repo="*"   — the agent default (cmd/agent/core/agent.go: LabelFilterRepo
//     = "*" "allow all repos by default"), overridable by an explicit custom
//     label;
//   - platform=<platform> — a real agent reports its own fused os/arch
//     (cmd/agent/core/agent.go stamps LabelFilterPlatform before the custom
//     labels), but the model has no agent to ask, so the value must be
//     configured. Overridable by an explicit custom label. When platform is
//     "" no platform key is synthesized at all: an unconfigured deployment
//     keeps today's behavior rather than asserting a platform it cannot know;
//   - org-id="*" — autoscaler-created agents are system agents (OrgID unset),
//     and the server enforces org-id="*" for them
//     (server/model/agent.go GetServerLabels).
//
// A custom label of the same key overrides the default (agent.go applies
// customLabels last via maps.Copy); org-id is server-enforced, so it is applied
// after the customs and always wins for the pool's system agents.
func NewPoolFilter(extra map[string]string, platform string) Filter {
	labels := make(map[string]string, len(extra)+synthesizedLabels)
	labels[pipeline.LabelFilterRepo] = "*"
	if platform != "" {
		labels[pipeline.LabelFilterPlatform] = platform
	}
	maps.Copy(labels, extra)
	// org-id is enforced by the server for system (autoscaler-created) agents,
	// so it is applied last and is not overridable by ExtraAgentLabels.
	labels[pipeline.LabelFilterOrg] = "*"
	return Filter{labels: labels}
}

// AgentFilter builds the modeled filter for an existing agent (used by the
// shared-demand netting step to test whether a non-pool/static agent can
// absorb a task). It mirrors the agent + server label synthesis:
//   - repo="*" default under the agent's CustomLabels;
//   - platform from the agent's own self-report. Unlike the pool's value this
//     needs no config: the agent already told the server its fused os/arch and
//     the API carries it as a first-class field, so it is truthful by
//     construction. It sits under CustomLabels for the same reason the real
//     agent's does (agent.go stamps it, then copies customs over the top);
//   - org-id from the server ownership rule (server/model/agent.go
//     GetServerLabels): OrgID unset (== idNotSet, -1) ⇒ "*", else the id.
func AgentFilter(a *woodpecker.Agent) Filter {
	labels := make(map[string]string, len(a.CustomLabels)+synthesizedLabels)
	labels[pipeline.LabelFilterRepo] = "*"
	if a.Platform != "" {
		labels[pipeline.LabelFilterPlatform] = a.Platform
	}
	maps.Copy(labels, a.CustomLabels)
	if a.OrgID != idNotSet {
		labels[pipeline.LabelFilterOrg] = strconv.FormatInt(a.OrgID, 10)
	} else {
		labels[pipeline.LabelFilterOrg] = "*"
	}
	return Filter{labels: labels}
}

// Satisfiable reports whether a task can run on an agent advertising this
// filter — the scheduler's own verdict, discarding the score.
func (f Filter) Satisfiable(t woodpecker.Task) bool {
	matched, _ := matchFilter(f.labels, t.Labels)
	return matched
}

// matchFilter is a verbatim transliteration of the server scheduler's
// createFilterFunc closure (server/scheduler/filter.go). It returns whether the
// agentLabels satisfy taskLabels and the scheduler's match score. Keep this
// byte-for-byte faithful to upstream; the parity test table guards it.
func matchFilter(agentLabels, taskLabels map[string]string) (bool, int) {
	// Create a copy of the labels for filtering to avoid modifying the original task.
	labels := maps.Clone(taskLabels)

	if requiredLabelsMissing(labels, agentLabels) {
		return false, 0
	}

	// ignore internal labels for filtering
	for k := range labels {
		if strings.HasPrefix(k, pipeline.InternalLabelPrefix) {
			delete(labels, k)
		}
	}

	score := 0
	for taskLabel, taskLabelValue := range labels {
		// if a task label is empty it will be ignored
		if taskLabelValue == "" {
			continue
		}

		// all task labels are required to be present for an agent to match
		agentLabelValue, ok := agentLabels[taskLabel]
		if !ok {
			// Check for required label
			agentLabelValue, ok = agentLabels["!"+taskLabel]
			if !ok {
				return false, 0
			}
		}

		switch agentLabelValue {
		// if agent label has a wildcard
		case "*":
			score++
		// if agent label has an exact match
		case taskLabelValue:
			score += 10
		// agent doesn't match
		default:
			return false, 0
		}
	}
	return true, score
}

// requiredLabelsMissing is a verbatim transliteration of the server scheduler's
// requiredLabelsMissing (server/scheduler/filter.go): every agent label
// prefixed "!" is a hard requirement — the task must carry that key with that
// exact value.
func requiredLabelsMissing(taskLabels, agentLabels map[string]string) bool {
	for label, value := range agentLabels {
		if len(label) > 0 && label[0] == '!' {
			val, ok := taskLabels[label[1:]]
			if !ok || val != value {
				return true
			}
		}
	}
	return false
}
