package api

import (
	"net/http"
	"sort"
	"time"

	"github.com/ClarifiedLabs/flow/internal/api/contract"
	"github.com/ClarifiedLabs/flow/internal/coordinator"
	flowharness "github.com/ClarifiedLabs/flow/internal/harness"
	"github.com/ClarifiedLabs/flow/internal/worker"
)

func (s *Server) handleHarnesses(w http.ResponseWriter, r *http.Request, principal coordinator.Principal) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	if !scopeAllowed(principal, coordinator.TokenScopeOwner, coordinator.TokenScopeConsole) {
		writeError(w, http.StatusForbidden, "forbidden", "harness options require owner or console token")
		return
	}

	workers, err := s.registry.Directory().ListWorkers(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list_workers_failed", err.Error())
		return
	}
	now := time.Now().UTC()
	liveWorkers := filterLiveWorkers(workers, now)
	availableAgents := flowharness.AgentDefinitionsFromLabels(liveWorkerHarnessLabels(liveWorkers))
	modelsByHarness := liveHarnessModelIntersection(liveWorkers)
	response := contract.HarnessesResponse{
		Agents:   harnessOptions(availableAgents, s.registry.HarnessArgs(), modelsByHarness),
		Consoles: harnessOptions(flowharness.ConsoleDefinitionsFromAvailableAgents(availableAgents), s.registry.HarnessArgs(), modelsByHarness),
	}
	writeJSON(w, http.StatusOK, response)
}

func harnessOptions(definitions []flowharness.Definition, defaultArgs flowharness.Args, modelsByHarness map[string][]flowharness.Model) []contract.HarnessOption {
	options := make([]contract.HarnessOption, 0, len(definitions))
	for _, definition := range definitions {
		option := contract.HarnessOption{
			Name:        definition.Name,
			DisplayName: definition.DisplayName,
			DefaultArgs: defaultArgs.For(definition.Name),
		}
		if models := modelsByHarness[definition.Name]; len(models) > 0 {
			option.Models = flowharness.CloneModels(models)
		}
		options = append(options, option)
	}
	return options
}

func filterLiveWorkers(workers []worker.Worker, now time.Time) []worker.Worker {
	live := make([]worker.Worker, 0, len(workers))
	for _, registeredWorker := range workers {
		if registeredWorker.ExpiresAt != nil && !registeredWorker.ExpiresAt.After(now) {
			continue
		}
		live = append(live, registeredWorker)
	}
	return live
}

func liveWorkerHarnessLabels(workers []worker.Worker) map[string]string {
	labels := map[string]string{}
	for _, registeredWorker := range workers {
		for key, value := range registeredWorker.Labels {
			if value == "true" {
				labels[key] = value
			}
		}
	}
	return labels
}

// harnessModelAccumulator intersects one harness's models across the live
// workers that offer that harness: a model is kept only when every offering
// worker advertises it.
type harnessModelAccumulator struct {
	workerCount int
	counts      map[string]int
	models      map[string]flowharness.Model
}

// liveHarnessModelIntersection computes, per harness name, the models common to
// all live workers offering that harness. Each model is grouped by its stamped
// Harness so claude/codex/harness catalogs stay separate, and the result is
// keyed by harness name for attachment to that harness's option.
func liveHarnessModelIntersection(workers []worker.Worker) map[string][]flowharness.Model {
	accumulators := map[string]*harnessModelAccumulator{}
	for _, registeredWorker := range workers {
		offered := map[string]bool{}
		for _, name := range flowharness.AgentNames() {
			if registeredWorker.Labels[flowharness.AgentHarnessLabel(name)] != "true" {
				continue
			}
			offered[name] = true
			acc := accumulators[name]
			if acc == nil {
				acc = &harnessModelAccumulator{counts: map[string]int{}, models: map[string]flowharness.Model{}}
				accumulators[name] = acc
			}
			acc.workerCount++
		}
		seen := map[string]bool{}
		for _, model := range registeredWorker.HarnessModels {
			harnessName := model.Harness
			if harnessName == "" || !offered[harnessName] {
				continue
			}
			id := model.QualifiedID
			if id == "" {
				id = model.ProviderID + ":" + model.ModelID
			}
			if id == "" {
				continue
			}
			dedupeKey := harnessName + "\x00" + id
			if seen[dedupeKey] {
				continue
			}
			seen[dedupeKey] = true
			acc := accumulators[harnessName]
			acc.counts[id]++
			if _, ok := acc.models[id]; !ok {
				acc.models[id] = model
			}
		}
	}

	out := map[string][]flowharness.Model{}
	for name, acc := range accumulators {
		models := make([]flowharness.Model, 0, len(acc.models))
		for id, model := range acc.models {
			if acc.counts[id] == acc.workerCount {
				models = append(models, model)
			}
		}
		if len(models) == 0 {
			continue
		}
		sort.SliceStable(models, func(i, j int) bool {
			if models[i].ProviderID != models[j].ProviderID {
				return models[i].ProviderID < models[j].ProviderID
			}
			return models[i].ModelID < models[j].ModelID
		})
		out[name] = models
	}
	return out
}
