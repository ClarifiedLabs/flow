package scheduler

import "testing"

func TestCompileSelectorNormalizesLabelsAndBareRequires(t *testing.T) {
	selector, err := CompileSelector(SelectorInput{
		RunsOn: map[string]string{
			"OS":   "Linux",
			"arch": "ARM64",
		},
		Requires: []string{"gpu", " docker "},
		Size:     "Medium",
	})
	if err != nil {
		t.Fatalf("compile selector: %v", err)
	}

	requirements := selector.Requirements()
	wantRequirements := Labels{
		"os":     "linux",
		"arch":   "arm64",
		"gpu":    "true",
		"docker": "true",
		"size":   "medium",
	}
	for key, want := range wantRequirements {
		if requirements[key] != want {
			t.Fatalf("requirement %s = %q, want %q; all requirements: %+v", key, requirements[key], want, requirements)
		}
	}

	labels, err := NormalizeLabels(map[string]string{
		"os":     "linux",
		"arch":   "arm64",
		"gpu":    "true",
		"docker": "true",
		"size":   "medium",
		"spot":   "false",
	})
	if err != nil {
		t.Fatalf("normalize labels: %v", err)
	}
	if !selector.Matches(labels) {
		t.Fatal("selector rejected worker with matching labels")
	}

	labels, err = NormalizeLabels(map[string]string{
		"os":   "linux",
		"arch": "arm64",
		"gpu":  "true",
		"size": "medium",
	})
	if err != nil {
		t.Fatalf("normalize missing label set: %v", err)
	}
	if selector.Matches(labels) {
		t.Fatal("selector matched worker missing docker=true")
	}
}

func TestSelectorRejectsConflictingRequirements(t *testing.T) {
	_, err := CompileSelector(SelectorInput{
		RunsOn:   map[string]string{"gpu": "false"},
		Requires: []string{"gpu"},
	})
	if err == nil {
		t.Fatal("compile selector accepted conflicting explicit and bare requirements")
	}
}

func TestNormalizeLabelsRejectsInvalidAndConflictingLabels(t *testing.T) {
	if _, err := NormalizeLabels(map[string]string{"os": "linux", " OS ": "macos"}); err == nil {
		t.Fatal("normalize labels accepted conflicting normalized keys")
	}

	if _, err := NormalizeLabels(map[string]string{"bad key": "linux"}); err == nil {
		t.Fatal("normalize labels accepted invalid key")
	}

	if _, err := NormalizeBareLabels([]string{"gpu=true"}); err == nil {
		t.Fatal("normalize bare labels accepted non-bare label")
	}
}

func TestTaintedWorkersRequireExactNoScheduleToleration(t *testing.T) {
	worker := Worker{
		Labels: map[string]string{"gpu": "true"},
		Taints: []Taint{{
			Key:    "lifetime",
			Value:  "persistent",
			Effect: EffectNoSchedule,
		}},
		Capacity: Capacity{PersistentAgent: 1},
	}
	selector, err := CompileSelector(SelectorInput{Requires: []string{"gpu"}})
	if err != nil {
		t.Fatalf("compile selector: %v", err)
	}

	job := Job{
		Selector:       selector,
		CapacityBucket: CapacityPersistentAgent,
	}
	eligible, err := Eligible(job, worker)
	if err != nil {
		t.Fatalf("check eligibility without toleration: %v", err)
	}
	if eligible {
		t.Fatal("tainted worker was eligible without toleration")
	}

	job.Tolerations = []Toleration{{
		Key:    "lifetime",
		Value:  "ephemeral",
		Effect: EffectNoSchedule,
	}}
	eligible, err = Eligible(job, worker)
	if err != nil {
		t.Fatalf("check eligibility with wrong toleration: %v", err)
	}
	if eligible {
		t.Fatal("tainted worker was eligible with wrong toleration value")
	}

	job.Tolerations = []Toleration{{
		Key:    "lifetime",
		Value:  "persistent",
		Effect: EffectNoSchedule,
	}}
	eligible, err = Eligible(job, worker)
	if err != nil {
		t.Fatalf("check eligibility with exact toleration: %v", err)
	}
	if !eligible {
		t.Fatal("tainted worker was not eligible with exact toleration")
	}
}

func TestUnsupportedTaintEffectsAreRejected(t *testing.T) {
	_, err := NormalizeTaint(Taint{
		Key:    "lifetime",
		Value:  "persistent",
		Effect: TaintEffect("NoExecute"),
	})
	if err == nil {
		t.Fatal("normalize taint accepted unsupported NoExecute effect")
	}
}

func TestCapacityBucketsAreIndependent(t *testing.T) {
	capacity := Capacity{
		PersistentAgent: 1,
		Ephemeral:       2,
	}

	hasRoom, err := capacity.HasRoom(CapacityPersistentAgent, Capacity{PersistentAgent: 1})
	if err != nil {
		t.Fatalf("check persistent capacity: %v", err)
	}
	if hasRoom {
		t.Fatal("persistent_agent bucket had room after persistent slot was consumed")
	}

	hasRoom, err = capacity.HasRoom(CapacityEphemeral, Capacity{PersistentAgent: 1})
	if err != nil {
		t.Fatalf("check ephemeral capacity: %v", err)
	}
	if !hasRoom {
		t.Fatal("ephemeral bucket was consumed by persistent_agent usage")
	}

	hasRoom, err = capacity.HasRoom(CapacityEphemeral, Capacity{Ephemeral: 2})
	if err != nil {
		t.Fatalf("check full ephemeral capacity: %v", err)
	}
	if hasRoom {
		t.Fatal("ephemeral bucket had room after ephemeral slots were consumed")
	}

	if _, err := ParseCapacityBucket("gpu"); err == nil {
		t.Fatal("parse capacity bucket accepted invalid bucket")
	}
}
