package scheduler

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

const trueLabelValue = "true"

type Labels map[string]string

type Selector struct {
	requirements Labels
}

type SelectorInput struct {
	RunsOn   map[string]string `json:"runs_on" yaml:"runs_on"`
	Requires []string          `json:"requires" yaml:"requires"`
	Size     string            `json:"size" yaml:"size"`
}

type TaintEffect string

const (
	EffectNoSchedule TaintEffect = "NoSchedule"
)

type Taint struct {
	Key    string      `json:"key" yaml:"key"`
	Value  string      `json:"value" yaml:"value"`
	Effect TaintEffect `json:"effect" yaml:"effect"`
}

type Toleration struct {
	Key    string      `json:"key" yaml:"key"`
	Value  string      `json:"value" yaml:"value"`
	Effect TaintEffect `json:"effect" yaml:"effect"`
}

type CapacityBucket string

const (
	CapacityPersistentAgent CapacityBucket = "persistent_agent"
	CapacityEphemeral       CapacityBucket = "ephemeral"
)

type Capacity struct {
	PersistentAgent int
	Ephemeral       int
}

type Worker struct {
	Labels   map[string]string
	Taints   []Taint
	Capacity Capacity
	Used     Capacity
}

type Job struct {
	Selector       Selector
	Tolerations    []Toleration
	CapacityBucket CapacityBucket
}

func NormalizeLabels(labels map[string]string) (Labels, error) {
	normalized := Labels{}
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, rawKey := range keys {
		key, err := normalizeLabelPart("label key", rawKey)
		if err != nil {
			return nil, err
		}
		value, err := normalizeLabelPart("label value", labels[rawKey])
		if err != nil {
			return nil, err
		}
		if err := putRequirement(normalized, key, value); err != nil {
			return nil, err
		}
	}

	return normalized, nil
}

func NormalizeBareLabels(labels []string) (Labels, error) {
	normalized := Labels{}
	for _, rawLabel := range labels {
		key, err := normalizeLabelPart("label key", rawLabel)
		if err != nil {
			return nil, err
		}
		if err := putRequirement(normalized, key, trueLabelValue); err != nil {
			return nil, err
		}
	}

	return normalized, nil
}

func CompileSelector(input SelectorInput) (Selector, error) {
	requirements, err := NormalizeLabels(input.RunsOn)
	if err != nil {
		return Selector{}, err
	}
	if strings.TrimSpace(input.Size) != "" {
		size, err := normalizeLabelPart("label value", input.Size)
		if err != nil {
			return Selector{}, err
		}
		if err := putRequirement(requirements, "size", size); err != nil {
			return Selector{}, err
		}
	}

	bare, err := NormalizeBareLabels(input.Requires)
	if err != nil {
		return Selector{}, err
	}
	for key, value := range bare {
		if err := putRequirement(requirements, key, value); err != nil {
			return Selector{}, err
		}
	}

	return Selector{requirements: requirements}, nil
}

func NewSelector(requirements map[string]string) (Selector, error) {
	normalized, err := NormalizeLabels(requirements)
	if err != nil {
		return Selector{}, err
	}

	return Selector{requirements: normalized}, nil
}

func (s Selector) Requirements() Labels {
	return cloneLabels(s.requirements)
}

func (s Selector) Matches(labels Labels) bool {
	for key, value := range s.requirements {
		if labels[key] != value {
			return false
		}
	}

	return true
}

func Eligible(job Job, worker Worker) (bool, error) {
	labels, err := NormalizeLabels(worker.Labels)
	if err != nil {
		return false, err
	}
	if !job.Selector.Matches(labels) {
		return false, nil
	}

	tolerated, err := TaintsTolerated(worker.Taints, job.Tolerations)
	if err != nil {
		return false, err
	}
	if !tolerated {
		return false, nil
	}

	return worker.Capacity.HasRoom(job.CapacityBucket, worker.Used)
}

func TaintsTolerated(taints []Taint, tolerations []Toleration) (bool, error) {
	normalizedTolerations := make([]Toleration, 0, len(tolerations))
	for _, toleration := range tolerations {
		normalized, err := NormalizeToleration(toleration)
		if err != nil {
			return false, err
		}
		normalizedTolerations = append(normalizedTolerations, normalized)
	}

	for _, taint := range taints {
		normalizedTaint, err := NormalizeTaint(taint)
		if err != nil {
			return false, err
		}
		if !taintTolerated(normalizedTaint, normalizedTolerations) {
			return false, nil
		}
	}

	return true, nil
}

func NormalizeTaint(taint Taint) (Taint, error) {
	key, err := normalizeLabelPart("taint key", taint.Key)
	if err != nil {
		return Taint{}, err
	}
	value, err := normalizeLabelPart("taint value", taint.Value)
	if err != nil {
		return Taint{}, err
	}
	effect, err := normalizeEffect(taint.Effect)
	if err != nil {
		return Taint{}, err
	}

	return Taint{
		Key:    key,
		Value:  value,
		Effect: effect,
	}, nil
}

func NormalizeToleration(toleration Toleration) (Toleration, error) {
	key, err := normalizeLabelPart("toleration key", toleration.Key)
	if err != nil {
		return Toleration{}, err
	}
	value, err := normalizeLabelPart("toleration value", toleration.Value)
	if err != nil {
		return Toleration{}, err
	}
	effect, err := normalizeEffect(toleration.Effect)
	if err != nil {
		return Toleration{}, err
	}

	return Toleration{
		Key:    key,
		Value:  value,
		Effect: effect,
	}, nil
}

func ParseCapacityBucket(value string) (CapacityBucket, error) {
	bucket := CapacityBucket(strings.ToLower(strings.TrimSpace(value)))
	switch bucket {
	case CapacityPersistentAgent, CapacityEphemeral:
		return bucket, nil
	default:
		return "", fmt.Errorf("invalid capacity bucket: %s", value)
	}
}

func (b CapacityBucket) Valid() bool {
	_, err := ParseCapacityBucket(string(b))
	return err == nil
}

func (c Capacity) Available(bucket CapacityBucket, used Capacity) (int, error) {
	total, err := c.value(bucket)
	if err != nil {
		return 0, err
	}
	consumed, err := used.value(bucket)
	if err != nil {
		return 0, err
	}
	if total < 0 {
		return 0, errors.New("capacity cannot be negative")
	}
	if consumed < 0 {
		return 0, errors.New("used capacity cannot be negative")
	}

	return total - consumed, nil
}

func (c Capacity) HasRoom(bucket CapacityBucket, used Capacity) (bool, error) {
	available, err := c.Available(bucket, used)
	if err != nil {
		return false, err
	}

	return available > 0, nil
}

func (c Capacity) value(bucket CapacityBucket) (int, error) {
	parsed, err := ParseCapacityBucket(string(bucket))
	if err != nil {
		return 0, err
	}

	switch parsed {
	case CapacityPersistentAgent:
		return c.PersistentAgent, nil
	case CapacityEphemeral:
		return c.Ephemeral, nil
	default:
		return 0, fmt.Errorf("invalid capacity bucket: %s", bucket)
	}
}

func putRequirement(labels Labels, key string, value string) error {
	if existing, ok := labels[key]; ok {
		if existing == value {
			return nil
		}
		return fmt.Errorf("conflicting label requirement for %s", key)
	}
	labels[key] = value
	return nil
}

func cloneLabels(labels Labels) Labels {
	cloned := make(Labels, len(labels))
	for key, value := range labels {
		cloned[key] = value
	}

	return cloned
}

func taintTolerated(taint Taint, tolerations []Toleration) bool {
	for _, toleration := range tolerations {
		if toleration.Key == taint.Key && toleration.Value == taint.Value && toleration.Effect == taint.Effect {
			return true
		}
	}

	return false
}

func normalizeEffect(effect TaintEffect) (TaintEffect, error) {
	normalized := TaintEffect(strings.TrimSpace(string(effect)))
	if normalized != EffectNoSchedule {
		return "", fmt.Errorf("unsupported taint effect: %s", effect)
	}

	return normalized, nil
}

func normalizeLabelPart(kind string, value string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(value))
	if normalized == "" {
		return "", fmt.Errorf("%s is required", kind)
	}
	for _, r := range normalized {
		valid := (r >= 'a' && r <= 'z') ||
			(r >= '0' && r <= '9') ||
			r == '-' ||
			r == '_' ||
			r == '.' ||
			r == '/'
		if !valid {
			return "", fmt.Errorf("invalid %s: %s", kind, value)
		}
	}

	return normalized, nil
}
