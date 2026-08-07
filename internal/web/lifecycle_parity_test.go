package web

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/ClarifiedLabs/flow/internal/coordinator"
)

// The board modules key lanes, phases, dwell and filters off lifecycle state
// strings. The canonical copy lives in assets/lifecycle.js (LIFECYCLE_STATES),
// and the server's truth is coordinator.AllLifecycleStates, the exhaustive
// enumeration of the LifecycleState constants in internal/coordinator/tasks.go.
// TestAllLifecycleStatesExhaustive in internal/coordinator keeps that
// enumeration in lockstep with the const declarations by parsing every
// LifecycleState constant in the package with go/parser — any file, any const
// block, any declaration form, including derived values like
// `LifecyclePaused = LifecycleState("paused")` — so the enumeration cannot
// silently lag the declarations either.
//
// This test iterates that enumeration directly rather than re-parsing Go
// source, so a new server constant is visible here the moment it exists: the
// JS set must be exactly the enumerated states plus the one documented
// client-side state (LIFECYCLE_UNSCHEDULED, the wire encoding of a task with
// no lifecycle state). Adding a server lifecycle state therefore fails this
// test until the JS vocabulary — and, via lifecycle.test.mjs, the board
// lanes/phases/filters — catch up, instead of silently rendering the new state
// as a wrong lane or label.
func TestLifecycleVocabularyParity(t *testing.T) {
	jsSource, err := os.ReadFile(filepath.Join("assets", "lifecycle.js"))
	if err != nil {
		t.Fatalf("read assets/lifecycle.js: %v", err)
	}

	// The server constants, referenced through the exhaustive enumeration:
	// renaming a constant changes its value here, and adding one grows this set
	// (once it is also listed in AllLifecycleStates, which the coordinator's
	// exhaustive test requires).
	serverStates := map[string]bool{}
	for _, state := range coordinator.AllLifecycleStates {
		serverStates[string(state)] = true
	}

	// Parse the JS string constants and the LIFECYCLE_STATES set (which must
	// reference those constants by name — the lifecycle.js comment says so).
	jsConsts := lifecycleJSConsts(t)
	jsSetMatch := lifecycleJSSetRE.FindStringSubmatch(string(jsSource))
	if jsSetMatch == nil {
		t.Fatalf("no LIFECYCLE_STATES = new Set([...]) found in assets/lifecycle.js")
	}
	jsStates := map[string]bool{}
	jsSetNames := map[string]bool{}
	for _, name := range strings.Split(jsSetMatch[1], ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		jsSetNames[name] = true
		value, ok := jsConsts[name]
		if !ok {
			t.Fatalf("LIFECYCLE_STATES references %q, which is not exported by assets/lifecycle.js", name)
		}
		jsStates[value] = true
	}

	// The JS set must be the Go constants plus exactly the documented
	// client-side state, LIFECYCLE_UNSCHEDULED.
	unscheduled, ok := jsConsts["LIFECYCLE_UNSCHEDULED"]
	if !ok {
		t.Fatalf("assets/lifecycle.js must export LIFECYCLE_UNSCHEDULED")
	}
	// Every exported LIFECYCLE_* string constant must be a member of the set:
	// exporting a state and forgetting to add it to LIFECYCLE_STATES is the
	// same drift the test exists to catch.
	constNames := make(map[string]bool, len(jsConsts))
	for name := range jsConsts {
		constNames[name] = true
	}
	if diff := sortedDiff(constNames, jsSetNames); len(diff) > 0 {
		t.Fatalf("assets/lifecycle.js exports lifecycle constants not in LIFECYCLE_STATES: %v", diff)
	}
	wantJS := map[string]bool{}
	for state := range serverStates {
		wantJS[state] = true
	}
	wantJS[unscheduled] = true
	if diff := sortedDiff(jsStates, wantJS); len(diff) > 0 {
		t.Fatalf("assets/lifecycle.js LIFECYCLE_STATES %v do not match the server LifecycleState constants plus LIFECYCLE_UNSCHEDULED (%q) (diff: %v); add the new state to the JS vocabulary and its lane/phase/filter mappings",
			sortedKeys(jsStates), unscheduled, diff)
	}
}

// TestLifecycleCallSitesUseTheVocabulary keeps the two remaining client
// surfaces that read lifecycle states honest: epic.js buckets epic members
// into working/queued/unscheduled groups and nav.js reads the /v2/sidebar
// lane counts. Both must derive those keys from assets/lifecycle.js, or a new
// server lifecycle state would silently mis-bucket them — the same drift
// TestLifecycleVocabularyParity catches for the board modules. lifecycle.test.mjs
// covers the rendering behavior; this test pins the call sites mechanically.
func TestLifecycleCallSitesUseTheVocabulary(t *testing.T) {
	jsConsts := lifecycleJSConsts(t)
	jsStates := map[string]bool{}
	for _, state := range jsConsts {
		jsStates[state] = true
	}

	epicSource, err := os.ReadFile(filepath.Join("assets", "elements", "epic.js"))
	if err != nil {
		t.Fatalf("read assets/elements/epic.js: %v", err)
	}
	navSource, err := os.ReadFile(filepath.Join("assets", "nav.js"))
	if err != nil {
		t.Fatalf("read assets/nav.js: %v", err)
	}

	// epic.js: memberState must read the member's state through
	// lifecycleStateOf and compare against the LIFECYCLE_* constants, and its
	// unscheduled bucket must be keyed by LIFECYCLE_UNSCHEDULED in STATE_PHASE.
	// Raw lifecycle literals in the bucketing body are the drift this test
	// exists to catch.
	epicBody := lifecycleEpicMemberStateBodyRE.FindStringSubmatch(string(epicSource))
	if epicBody == nil {
		t.Fatalf("no memberState(member) { ... } body found in assets/elements/epic.js (structure changed? update the parity test)")
	}
	for _, literal := range []string{`"scheduled"`, `"in_progress"`, `"unscheduled"`, `"done"`} {
		if strings.Contains(epicBody[1], literal) {
			t.Fatalf("assets/elements/epic.js memberState hand-writes the lifecycle literal %s; read the member state through lifecycleStateOf and compare against the LIFECYCLE_* constants", literal)
		}
	}
	for _, token := range []string{"lifecycleStateOf", "LIFECYCLE_IN_PROGRESS", "LIFECYCLE_SCHEDULED", "LIFECYCLE_UNSCHEDULED"} {
		if !strings.Contains(epicBody[1], token) {
			t.Fatalf("assets/elements/epic.js memberState does not reference %s; the epic buckets must derive from the shared lifecycle vocabulary", token)
		}
	}
	if !lifecycleEpicUnscheduledPhaseRE.MatchString(string(epicSource)) {
		t.Fatalf("assets/elements/epic.js STATE_PHASE must key the unscheduled bucket with [LIFECYCLE_UNSCHEDULED]")
	}

	// nav.js: the board lane-count tuples must key the lifecycle lanes with
	// LIFECYCLE_* constants — the only literal key left is the non-lifecycle
	// blocked lane — and the Tasks and Done badges must read their counts
	// through LIFECYCLE_UNSCHEDULED and LIFECYCLE_DONE.
	tuples := lifecycleNavCountTupleRE.FindAllStringSubmatch(string(navSource), -1)
	if len(tuples) < 3 {
		t.Fatalf("expected at least 3 lane-count tuples in assets/nav.js renderNavStatus, found %d (structure changed? update the parity test)", len(tuples))
	}
	lifecycleLanes := map[string]string{} // lifecycle state -> source token
	for _, tuple := range tuples {
		key := tuple[1]
		if strings.HasPrefix(key, "LIFECYCLE_") {
			state, ok := jsConsts[key]
			if !ok {
				t.Fatalf("assets/nav.js lane-count key %q is not exported by assets/lifecycle.js", key)
			}
			lifecycleLanes[state] = key
			continue
		}
		if jsStates[strings.Trim(key, `"`)] {
			t.Fatalf("assets/nav.js lane-count key %s must come from a LIFECYCLE_* constant, not a raw lifecycle literal", key)
		}
	}
	for _, state := range []string{"scheduled", "in_progress"} {
		if lifecycleLanes[state] == "" {
			t.Fatalf("assets/nav.js must key the %q lane count with a LIFECYCLE_* constant (lifecycle lanes found: %v)", state, lifecycleLanes)
		}
	}
	unscheduledCount := lifecycleNavUnscheduledCountRE.FindStringSubmatch(string(navSource))
	if unscheduledCount == nil || unscheduledCount[1] != "LIFECYCLE_UNSCHEDULED" {
		t.Fatalf("assets/nav.js must read the unscheduled Tasks badge count through LIFECYCLE_UNSCHEDULED")
	}
	doneCount := lifecycleNavDoneCountRE.FindStringSubmatch(string(navSource))
	if doneCount == nil || doneCount[1] != "LIFECYCLE_DONE" {
		t.Fatalf("assets/nav.js must read the Done badge count through LIFECYCLE_DONE")
	}
}

// lifecycleJSConsts parses the LIFECYCLE_* string constants out of
// assets/lifecycle.js. Every exported constant is a vocabulary state (the
// set-membership invariant is enforced in TestLifecycleVocabularyParity).
func lifecycleJSConsts(t *testing.T) map[string]string {
	t.Helper()
	jsSource, err := os.ReadFile(filepath.Join("assets", "lifecycle.js"))
	if err != nil {
		t.Fatalf("read assets/lifecycle.js: %v", err)
	}
	jsConsts := map[string]string{}
	for _, match := range lifecycleJSConstRE.FindAllStringSubmatch(string(jsSource), -1) {
		jsConsts[match[1]] = match[2]
	}
	return jsConsts
}

func TestLifecycleControlTargetsCoverTransitions(t *testing.T) {
	taskModelSource, err := os.ReadFile(filepath.Join("assets", "task-model.js"))
	if err != nil {
		t.Fatalf("read assets/task-model.js: %v", err)
	}
	// Extract LIFECYCLE_TARGET_OPTIONS values: { value: "..." } entries.
	targetRE := regexp.MustCompile(`value:\s*"([^"]+)"`)
	rawTargets := map[string]bool{}
	sectionRE := regexp.MustCompile(`(?s)LIFECYCLE_TARGET_OPTIONS\s*=\s*\[([^\]]*)\]`)
	section := sectionRE.FindStringSubmatch(string(taskModelSource))
	if section == nil {
		t.Fatalf("no LIFECYCLE_TARGET_OPTIONS found in assets/task-model.js")
	}
	for _, m := range targetRE.FindAllStringSubmatch(section[1], -1) {
		rawTargets[m[1]] = true
	}
	// Bare values may include done:<resolution> form; normalize to the check vocabulary.
	normalized := map[string]bool{}
	for target := range rawTargets {
		normalized[strings.ToLower(strings.TrimSpace(target))] = true
	}
	// Every non-derived lifecycle transition target must be reachable from the control.
	// Derived phases (critique/acceptance/approved/merged_closed/...) remain valid
	// vocabulary on the server but cannot be set directly; they are allowed to be
	// present but not required — the check is that no actionable target is missing.
	required := []string{
		"backlog", "up_next", "scheduled", "working", "reopen", "retry", "skip", "hold", "resume", "reset", "schedule", "scheduled",
		"done:completed", "done:rejected", "done:abandoned", "done:cancelled", "done:failed", "triage", "unscheduled",
	}
	for _, need := range required {
		if !normalized[need] {
			t.Fatalf("lifecycle control missing required target %q (LIFECYCLE_TARGET_OPTIONS=%v)", need, sortedKeys(normalized))
		}
	}
	// Every option must be a valid server lifecycle transition target (or a done:<resolution>).
	for target := range rawTargets {
		lc := strings.ToLower(strings.TrimSpace(target))
		if strings.HasPrefix(lc, "done:") {
			res := strings.TrimPrefix(lc, "done:")
			if _, ok := map[string]bool{"completed": true, "rejected": true, "abandoned": true, "cancelled": true, "failed": true, "merged": true}[res]; !ok {
				t.Fatalf("lifecycle control has unknown done resolution %q", target)
			}
			continue
		}
		if !coordinator.IsValidLifecycleTarget(lc) {
			t.Fatalf("lifecycle control target %q is not a valid server lifecycle transition target", target)
		}
	}
}

var (
	// lifecycleJSConstRE matches `export const LIFECYCLE_X = "value"`.
	lifecycleJSConstRE = regexp.MustCompile(`export const (LIFECYCLE_[A-Z_]+) = "([^"]+)"`)
	// lifecycleJSSetRE matches the LIFECYCLE_STATES set literal.
	lifecycleJSSetRE = regexp.MustCompile(`export const LIFECYCLE_STATES = new Set\(\[([^\]]*)\]\)`)
	// lifecycleEpicMemberStateBodyRE matches the body of memberState in
	// assets/elements/epic.js (the function currently has no nested braces).
	lifecycleEpicMemberStateBodyRE = regexp.MustCompile(`(?s)function memberState\(member\) \{\n(.*?)\n\}`)
	// lifecycleEpicUnscheduledPhaseRE matches the STATE_PHASE entry for the
	// unscheduled bucket, which must be keyed by the vocabulary constant.
	lifecycleEpicUnscheduledPhaseRE = regexp.MustCompile(`\[\s*LIFECYCLE_UNSCHEDULED\s*\]:\s*"[^"]+"`)
	// lifecycleNavCountTupleRE matches one [key, GoField, label] lane-count
	// tuple in assets/nav.js renderNavStatus.
	lifecycleNavCountTupleRE = regexp.MustCompile(`\[\s*(LIFECYCLE_[A-Z_]+|"[a-z_]+")\s*,\s*"[A-Za-z]+"\s*,\s*"[^"]+"\s*\]`)
	// lifecycleNavUnscheduledCountRE matches the Tasks badge count read.
	lifecycleNavUnscheduledCountRE = regexp.MustCompile(`value\(board,\s*(LIFECYCLE_UNSCHEDULED|"[^"]*")\s*,\s*"Unscheduled"\)`)
	// lifecycleNavDoneCountRE matches the Done badge count read.
	lifecycleNavDoneCountRE = regexp.MustCompile(`value\(status,\s*(LIFECYCLE_DONE|"[^"]*")\s*,\s*"Done"\)`)
)

func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// sortedDiff returns the keys present in exactly one of the two sets, sorted.
func sortedDiff(left, right map[string]bool) []string {
	var diff []string
	for key := range left {
		if !right[key] {
			diff = append(diff, key)
		}
	}
	for key := range right {
		if !left[key] {
			diff = append(diff, key)
		}
	}
	sort.Strings(diff)
	return diff
}
