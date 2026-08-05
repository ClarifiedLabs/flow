// Package orchestrator reconciles durable coordinator capacity slots with
// one-shot worker resources and project-local assignments.
package orchestrator

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ClarifiedLabs/flow/internal/api/contract"
	flowharness "github.com/ClarifiedLabs/flow/internal/harness"
	"github.com/ClarifiedLabs/flow/internal/metrics"
	"github.com/ClarifiedLabs/flow/internal/scheduler"
	"github.com/ClarifiedLabs/flow/internal/worker"
)

// ProviderStatus is the observed state of one one-shot provider resource.
type ProviderStatus string

const (
	ProviderNotFound   ProviderStatus = "not-found"
	ProviderIncomplete ProviderStatus = "incomplete"
	ProviderPending    ProviderStatus = "pending"
	ProviderRunning    ProviderStatus = "running"
	ProviderSucceeded  ProviderStatus = "succeeded"
	ProviderFailed     ProviderStatus = "failed"
)

// AssignmentIdentity is the stable identity passed to provider operations.
type AssignmentIdentity struct {
	AssignmentID      string
	WorkerID          string
	ProviderID        string
	ProfileName       string
	ProviderRequestID string
	ProviderType      string
	ProviderOptions   map[string]string
}

// IdentityOf returns the provider identity persisted in an assignment.
func IdentityOf(value contract.ProvisionerAssignment) AssignmentIdentity {
	a := value.Assignment
	return AssignmentIdentity{
		AssignmentID: a.ID, WorkerID: a.WorkerID, ProviderID: a.ProviderID,
		ProfileName: a.ProfileName, ProviderRequestID: a.ProviderRequestID,
		ProviderType: a.ProviderType, ProviderOptions: a.ProviderOptions,
	}
}

func IdentityOfSlot(slot worker.CapacitySlot) AssignmentIdentity {
	return AssignmentIdentity{
		AssignmentID: slot.ID, WorkerID: slot.WorkerID, ProviderID: slot.ProviderID,
		ProfileName: slot.ProfileName, ProviderRequestID: slot.ProviderRequestID,
		ProviderType: slot.ProviderType, ProviderOptions: slot.ProviderOptions,
	}
}

// LaunchRequest contains everything a provider needs to start one direct,
// assignment-bound worker. WorkerToken must only be written to private config.
type LaunchRequest struct {
	Identity       AssignmentIdentity
	Assignment     contract.ProvisionerAssignment
	Slot           *worker.CapacitySlot
	Profile        Profile
	CoordinatorURL string
	WorkerToken    string
}

func (p Profile) capacitySlotRequest(requestID string) contract.CreateProvisionerCapacitySlotRequest {
	seconds := int(p.StartupTimeout / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	return contract.CreateProvisionerCapacitySlotRequest{
		ProviderID: p.ProviderID, ProviderRequestID: requestID,
		ProfileName: p.ProfileName, ProviderType: p.ProviderType,
		ProviderOptions: p.ProviderOptions, MaxInstances: p.MaxConcurrency + p.IdleCapacity,
		AllowedRoles: p.AllowedRoles, AllowedBuckets: p.AllowedBuckets,
		Labels: p.Labels, Taints: p.Taints, HarnessModels: p.HarnessModels,
		RequiredSelector: p.RequiredSelector, StartupTimeoutSeconds: seconds,
	}
}

func (p Profile) capacityDemandRequest() contract.ProvisionerCapacityDemandRequest {
	return contract.ProvisionerCapacityDemandRequest{
		ProviderID: p.ProviderID, ProfileName: p.ProfileName,
		MaxConcurrency: p.MaxConcurrency, IdleCapacity: p.IdleCapacity,
		AllowedRoles: p.AllowedRoles, AllowedBuckets: p.AllowedBuckets,
		Labels: p.Labels, Taints: p.Taints, HarnessModels: p.HarnessModels,
		RequiredSelector: p.RequiredSelector,
	}
}

// Provider manages exactly one resource per stable assignment identity.
type Provider interface {
	Health(context.Context, Profile) error
	Launch(context.Context, LaunchRequest) error
	Inspect(context.Context, AssignmentIdentity) (ProviderStatus, error)
	Delete(context.Context, AssignmentIdentity) error
}

// PermanentError marks a provider validation or authorization error for which
// retrying the same assignment cannot help.
type PermanentError struct{ Err error }

func (e *PermanentError) Error() string { return e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

// Permanent marks err as non-retryable.
func Permanent(err error) error {
	if err == nil || IsPermanent(err) {
		return err
	}
	return &PermanentError{Err: err}
}

// IsPermanent reports whether a provider error is non-retryable.
func IsPermanent(err error) bool {
	var target *PermanentError
	return errors.As(err, &target)
}

// Profile is an immutable provisioner scheduling and provider profile.
type Profile struct {
	ProviderID       string
	ProfileName      string
	MaxConcurrency   int
	IdleCapacity     int
	AllowedRoles     []worker.JobRole
	AllowedBuckets   []worker.CapacityBucket
	Labels           map[string]string
	Taints           []scheduler.Taint
	HarnessModels    []flowharness.Model
	RequiredSelector map[string]string
	StartupTimeout   time.Duration
	Provider         string
	ProviderType     string
	ProviderOptions  map[string]string
}

func (p Profile) reserveRequest(requestID string) contract.ReserveProvisionerAssignmentRequest {
	seconds := int(p.StartupTimeout / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	return contract.ReserveProvisionerAssignmentRequest{
		ProviderID: p.ProviderID, ProviderRequestID: requestID,
		ProfileName: p.ProfileName, ProviderType: p.ProviderType, ProviderOptions: p.ProviderOptions,
		MaxConcurrency: p.MaxConcurrency,
		AllowedRoles:   p.AllowedRoles, AllowedBuckets: p.AllowedBuckets,
		Labels: p.Labels, Taints: p.Taints, HarnessModels: p.HarnessModels,
		RequiredSelector: p.RequiredSelector, StartupTimeoutSeconds: seconds,
	}
}

// Metrics contains assignment-centric metric handles. Nil handles are ignored.
type Metrics struct {
	ReserveErrors     *metrics.Counter
	LaunchErrors      *metrics.Counter
	CleanupErrors     *metrics.Counter
	ReconcileErrors   *metrics.Counter
	ActiveAssignments *metrics.Gauge
}

// NewMetrics registers the assignment reconciler's metric families.
func NewMetrics(registry *metrics.Registry) Metrics {
	if registry == nil {
		return Metrics{}
	}
	return Metrics{
		ReserveErrors:     registry.Counter("flow_orchestrator_reserve_errors_total", "Coordinator assignment reservation errors."),
		LaunchErrors:      registry.Counter("flow_orchestrator_launch_errors_total", "Assignment provider launch errors."),
		CleanupErrors:     registry.Counter("flow_orchestrator_cleanup_errors_total", "Assignment provider cleanup errors."),
		ReconcileErrors:   registry.Counter("flow_orchestrator_reconcile_errors_total", "Assignment reconciliation errors by operation."),
		ActiveAssignments: registry.Gauge("flow_orchestrator_active_assignments", "Open durable assignments by provider, profile, and state."),
	}
}

// ReconcilerOptions configures a Reconciler.
type ProviderFactory func(Profile) (Provider, error)

type ReconcilerOptions struct {
	Coordinator     Coordinator
	CoordinatorURL  string
	Profiles        []Profile
	Providers       map[string]Provider
	ProviderFactory ProviderFactory
	PollInterval    time.Duration
	RetryBase       time.Duration
	RetryMax        time.Duration
	Metrics         Metrics
	Now             func() time.Time
	NewRequestID    func() (string, error)
}

// Reconciler recovers durable assignments before reserving new work.
type Reconciler struct {
	coordinator     Coordinator
	coordinatorURL  string
	profiles        []Profile
	providerIDs     []string
	profileByID     map[string]Profile
	providers       map[string]Provider
	providerFactory ProviderFactory
	pollInterval    time.Duration
	retryBase       time.Duration
	retryMax        time.Duration
	metrics         Metrics
	now             func() time.Time
	newRequestID    func() (string, error)
	ready           atomic.Bool
	activeSeries    map[string]map[string]string
}

// NewReconciler validates and constructs an assignment reconciler.
func NewReconciler(options ReconcilerOptions) (*Reconciler, error) {
	if options.Coordinator == nil {
		return nil, errors.New("orchestrator coordinator is required")
	}
	if strings.TrimSpace(options.CoordinatorURL) == "" {
		return nil, errors.New("orchestrator coordinator URL is required")
	}
	if len(options.Profiles) == 0 {
		return nil, errors.New("at least one orchestrator profile is required")
	}
	if options.PollInterval <= 0 {
		options.PollInterval = 5 * time.Second
	}
	if options.RetryBase <= 0 {
		options.RetryBase = time.Second
	}
	if options.RetryMax <= 0 {
		options.RetryMax = time.Minute
	}
	if options.RetryMax < options.RetryBase {
		return nil, errors.New("orchestrator retry max must not be less than retry base")
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.NewRequestID == nil {
		options.NewRequestID = randomProviderRequestID
	}
	profileByID := make(map[string]Profile, len(options.Profiles))
	providerIDSet := make(map[string]struct{}, len(options.Profiles))
	normalizedProfiles := make([]Profile, 0, len(options.Profiles))
	for _, profile := range options.Profiles {
		profile.ProviderID = strings.TrimSpace(profile.ProviderID)
		profile.ProfileName = strings.TrimSpace(profile.ProfileName)
		profile.Provider = strings.TrimSpace(profile.Provider)
		profile.ProviderType = strings.TrimSpace(profile.ProviderType)
		if profile.ProviderID == "" || profile.ProfileName == "" || profile.Provider == "" || profile.ProviderType == "" {
			return nil, errors.New("profile provider id, profile name, provider selection, and provider type are required")
		}
		if profile.MaxConcurrency <= 0 || profile.StartupTimeout <= 0 {
			return nil, fmt.Errorf("profile %s/%s requires positive max concurrency and startup timeout", profile.ProviderID, profile.ProfileName)
		}
		if profile.IdleCapacity < 0 {
			return nil, fmt.Errorf("profile %s/%s requires non-negative idle capacity", profile.ProviderID, profile.ProfileName)
		}
		if options.Providers[profile.Provider] == nil {
			return nil, fmt.Errorf("profile %s/%s selects unconfigured provider %q", profile.ProviderID, profile.ProfileName, profile.Provider)
		}
		key := profileKey(profile.ProviderID, profile.ProfileName)
		if _, exists := profileByID[key]; exists {
			return nil, fmt.Errorf("duplicate profile %s/%s", profile.ProviderID, profile.ProfileName)
		}
		profileByID[key] = profile
		providerIDSet[profile.ProviderID] = struct{}{}
		normalizedProfiles = append(normalizedProfiles, profile)
	}
	providerIDs := make([]string, 0, len(providerIDSet))
	for providerID := range providerIDSet {
		providerIDs = append(providerIDs, providerID)
	}
	sort.Strings(providerIDs)
	return &Reconciler{
		coordinator: options.Coordinator, coordinatorURL: strings.TrimRight(strings.TrimSpace(options.CoordinatorURL), "/"),
		profiles: normalizedProfiles, providerIDs: providerIDs, profileByID: profileByID,
		providers: options.Providers, providerFactory: options.ProviderFactory, pollInterval: options.PollInterval,
		retryBase: options.RetryBase, retryMax: options.RetryMax,
		metrics: options.Metrics, now: options.Now, newRequestID: options.NewRequestID,
		activeSeries: make(map[string]map[string]string),
	}, nil
}

// Ready reports whether one complete recovery-and-reservation cycle succeeded.
func (r *Reconciler) Ready() bool { return r.ready.Load() }

// Run reconciles immediately and then at PollInterval until cancellation.
func (r *Reconciler) Run(ctx context.Context) error {
	if ctx.Err() != nil {
		return nil
	}
	r.runCycle(ctx)
	ticker := time.NewTicker(r.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			r.runCycle(ctx)
		}
	}
}

func (r *Reconciler) runCycle(ctx context.Context) {
	r.ready.Store(false)
	if err := r.Reconcile(ctx); err != nil {
		if ctx.Err() == nil {
			slog.Warn("orchestrator assignment reconciliation failed", "error", err)
		}
		return
	}
	r.ready.Store(true)
}

// Reconcile performs one recovery-first cycle. It is exported for synchronous
// startup integration and deterministic tests.
func (r *Reconciler) Reconcile(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if capacity, ok := r.coordinator.(CapacityCoordinator); ok {
		return r.reconcileCapacity(ctx, capacity)
	}
	assignments, err := r.coordinator.ListAssignments(ctx, r.providerIDs)
	if err != nil {
		r.reconcileError("list", "", "")
		return fmt.Errorf("list durable assignments: %w", err)
	}
	r.updateActiveMetrics(assignments)

	var cycleErrors []error
	blocked := make(map[string]bool)
	for _, profile := range r.profiles {
		provider := r.providers[profile.Provider]
		if err := provider.Health(ctx, profile); err != nil {
			key := profileKey(profile.ProviderID, profile.ProfileName)
			blocked[key] = true
			r.reconcileError("health", profile.ProviderID, profile.ProfileName)
			cycleErrors = append(cycleErrors, fmt.Errorf("check provider %s for profile %s/%s: %w", profile.Provider, profile.ProviderID, profile.ProfileName, err))
		}
	}

	open := make(map[string]int)
	for _, assignment := range assignments {
		key := profileKey(assignment.Assignment.ProviderID, assignment.Assignment.ProfileName)
		configured, isConfigured := r.profileByID[key]
		if blocked[key] && assignment.Assignment.State == worker.AssignmentPending && isConfigured && profileMatchesAssignment(configured, assignment.Assignment) {
			open[key]++
			continue
		}
		assignmentOpen, err := r.recoverAssignment(ctx, assignment)
		if assignmentOpen {
			open[key]++
		}
		if err != nil {
			blocked[key] = true
			cycleErrors = append(cycleErrors, err)
		}
	}

	// Recovery completes per profile before that profile can reserve new work.
	for _, profile := range r.profiles {
		key := profileKey(profile.ProviderID, profile.ProfileName)
		if blocked[key] {
			continue
		}
		for open[key] < profile.MaxConcurrency {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			reserved, assignmentOpen, err := r.reserveAndLaunch(ctx, profile)
			if err != nil {
				cycleErrors = append(cycleErrors, err)
				break
			}
			if !reserved {
				break
			}
			if !assignmentOpen {
				// A permanent provider failure was fully abandoned. Avoid a hot
				// loop against the same broken profile until the next poll.
				break
			}
			open[key]++
		}
	}
	return errors.Join(cycleErrors...)
}

func (r *Reconciler) recoverAssignment(ctx context.Context, assignment contract.ProvisionerAssignment) (bool, error) {
	if err := ctx.Err(); err != nil {
		return assignment.Assignment.State != worker.AssignmentClosed, err
	}
	a := assignment.Assignment
	if a.State == worker.AssignmentClosed {
		if a.CleanedAt != nil {
			return false, nil
		}
		if a.CredentialsRevokedAt == nil {
			if _, err := r.coordinator.Revoke(ctx, a.ID); err != nil {
				r.cleanupError(Profile{ProviderID: a.ProviderID, ProfileName: a.ProfileName})
				return false, fmt.Errorf("revoke closed assignment %s credentials: %w", a.ID, err)
			}
		}
	}
	if a.State == worker.AssignmentClaimed {
		// Once claimed, the ordinary lease lifecycle owns the work. Provider
		// availability must not gate recovery or unrelated reservations.
		return true, nil
	}
	profile, provider, launchAllowed, err := r.recoveryProvider(assignment)
	if err != nil {
		r.reconcileError("profile", a.ProviderID, a.ProfileName)
		return a.State != worker.AssignmentClosed, fmt.Errorf("recover assignment %s: %w", a.ID, err)
	}
	identity := IdentityOf(assignment)
	if a.State == worker.AssignmentClosed {
		if err := provider.Delete(ctx, identity); err != nil {
			r.cleanupError(profile)
			return false, fmt.Errorf("delete closed assignment %s: %w", a.ID, err)
		}
		if _, err := r.coordinator.Cleaned(ctx, a.ID); err != nil {
			r.cleanupError(profile)
			return false, fmt.Errorf("mark assignment %s cleaned: %w", a.ID, err)
		}
		return false, nil
	}

	status, err := provider.Inspect(ctx, identity)
	if err != nil {
		r.reconcileError("inspect", a.ProviderID, a.ProfileName)
		return true, fmt.Errorf("inspect assignment %s: %w", a.ID, err)
	}
	if a.State != worker.AssignmentPending {
		r.reconcileError("state", a.ProviderID, a.ProfileName)
		return true, fmt.Errorf("assignment %s has unsupported state %q", a.ID, a.State)
	}

	if !a.StartupDeadline.IsZero() && !r.now().Before(a.StartupDeadline) {
		err := r.abandonPending(ctx, provider, profile, assignment, "startup deadline elapsed", true)
		return err != nil, err
	}
	switch status {
	case ProviderPending, ProviderRunning:
		return true, nil
	case ProviderIncomplete:
		if !launchAllowed {
			err := r.abandonPending(ctx, provider, profile, assignment, "persisted provider descriptor is not approved by current configuration", true)
			return err != nil, err
		}
		return r.relaunchIncomplete(ctx, provider, profile, assignment)
	case ProviderSucceeded:
		err := r.abandonPending(ctx, provider, profile, assignment, "worker exited before claiming assignment", true)
		return err != nil, err
	case ProviderFailed:
		err := r.abandonPending(ctx, provider, profile, assignment, "provider resource failed before claim", true)
		return err != nil, err
	case ProviderNotFound:
		if !launchAllowed {
			return false, r.rejectUnapprovedAssignment(ctx, profile, assignment, "persisted provider descriptor is not approved by current configuration")
		}
		if a.NextRetryAt != nil && r.now().Before(*a.NextRetryAt) {
			return true, nil
		}
		return r.launchPending(ctx, provider, profile, assignment)
	default:
		r.reconcileError("inspect_status", a.ProviderID, a.ProfileName)
		return true, fmt.Errorf("inspect assignment %s returned unknown status %q", a.ID, status)
	}
}

func (r *Reconciler) launchPending(ctx context.Context, provider Provider, profile Profile, assignment contract.ProvisionerAssignment) (bool, error) {
	refreshed, token, err := r.refreshPendingReservation(ctx, profile, assignment)
	if err != nil {
		return true, err
	}
	return r.launch(ctx, provider, profile, refreshed, token)
}

// relaunchIncomplete fences the credential in the partial provider resource
// before deleting it. A crash after reservation refresh can therefore leave only
// a stale Secret/config behind, never a deleted Secret with a still-live token.
func (r *Reconciler) relaunchIncomplete(ctx context.Context, provider Provider, profile Profile, assignment contract.ProvisionerAssignment) (bool, error) {
	refreshed, token, err := r.refreshPendingReservation(ctx, profile, assignment)
	if err != nil {
		return true, err
	}
	if err := provider.Delete(ctx, IdentityOf(assignment)); err != nil {
		r.cleanupError(profile)
		return true, fmt.Errorf("delete incomplete provider resource for assignment %s: %w", assignment.Assignment.ID, err)
	}
	return r.launch(ctx, provider, profile, refreshed, token)
}

func (r *Reconciler) refreshPendingReservation(ctx context.Context, profile Profile, assignment contract.ProvisionerAssignment) (contract.ProvisionerAssignment, string, error) {
	a := assignment.Assignment
	response, err := r.coordinator.Reserve(ctx, profile.reserveRequest(a.ProviderRequestID))
	if err != nil {
		r.reserveError(profile)
		return contract.ProvisionerAssignment{}, "", fmt.Errorf("refresh reservation %s token: %w", a.ID, err)
	}
	if !response.Reserved || response.Assignment == nil || response.Assignment.Assignment.ID != a.ID ||
		!reflect.DeepEqual(IdentityOf(*response.Assignment), IdentityOf(assignment)) ||
		response.Assignment.Assignment.State != worker.AssignmentPending || strings.TrimSpace(response.WorkerToken) == "" {
		r.reserveError(profile)
		return contract.ProvisionerAssignment{}, "", fmt.Errorf("refresh reservation %s returned inconsistent assignment or empty token", a.ID)
	}
	return *response.Assignment, response.WorkerToken, nil
}

func (r *Reconciler) reserveAndLaunch(ctx context.Context, profile Profile) (bool, bool, error) {
	requestID, err := r.newRequestID()
	if err != nil {
		r.reserveError(profile)
		return false, false, fmt.Errorf("generate provider request id: %w", err)
	}
	response, err := r.coordinator.Reserve(ctx, profile.reserveRequest(requestID))
	if err != nil {
		r.reserveError(profile)
		return false, false, fmt.Errorf("reserve assignment for %s/%s: %w", profile.ProviderID, profile.ProfileName, err)
	}
	if !response.Reserved {
		return false, false, nil
	}
	if response.Assignment == nil || strings.TrimSpace(response.WorkerToken) == "" {
		r.reserveError(profile)
		return false, false, errors.New("coordinator reserved assignment without assignment identity or worker token")
	}
	a := response.Assignment.Assignment
	if a.ProviderRequestID != requestID || a.ProviderID != profile.ProviderID || a.ProfileName != profile.ProfileName {
		r.reserveError(profile)
		return false, false, fmt.Errorf("coordinator returned mismatched assignment %s", a.ID)
	}
	assignmentOpen, err := r.launch(ctx, r.providers[profile.Provider], profile, *response.Assignment, response.WorkerToken)
	return true, assignmentOpen, err
}

func (r *Reconciler) launch(ctx context.Context, provider Provider, profile Profile, assignment contract.ProvisionerAssignment, token string) (bool, error) {
	a := assignment.Assignment
	if !profileMatchesAssignment(profile, a) {
		return false, r.rejectUnapprovedAssignment(ctx, profile, assignment, "coordinator returned a provider descriptor that does not match local configuration")
	}
	identity := IdentityOf(assignment)
	identity.ProviderType = profile.ProviderType
	identity.ProviderOptions = profile.ProviderOptions
	err := provider.Launch(ctx, LaunchRequest{
		Identity: identity, Assignment: assignment, Profile: profile,
		CoordinatorURL: r.coordinatorURL, WorkerToken: token,
	})
	if err != nil {
		r.launchError(profile)
		if IsPermanent(err) {
			if abandonErr := r.abandonPending(ctx, provider, profile, assignment, err.Error(), true); abandonErr != nil {
				return true, errors.Join(fmt.Errorf("permanent launch failure for %s: %w", a.ID, err), abandonErr)
			}
			return false, nil
		}
		next := r.now().Add(r.retryDelay(a.RetryCount))
		_, recordErr := r.coordinator.RecordAttempt(ctx, a.ID, contract.RecordProvisionerAssignmentAttemptRequest{
			ProviderError: err.Error(), NextRetryAt: &next,
		})
		if recordErr != nil {
			r.reconcileError("attempt", profile.ProviderID, profile.ProfileName)
			return true, errors.Join(fmt.Errorf("launch assignment %s: %w", a.ID, err), fmt.Errorf("record launch retry: %w", recordErr))
		}
		return true, fmt.Errorf("launch assignment %s: %w", a.ID, err)
	}
	if _, err := r.coordinator.RecordAttempt(ctx, a.ID, contract.RecordProvisionerAssignmentAttemptRequest{}); err != nil {
		r.reconcileError("attempt", profile.ProviderID, profile.ProfileName)
		return true, fmt.Errorf("record successful launch attempt for %s: %w", a.ID, err)
	}
	return true, nil
}

func (r *Reconciler) abandonPending(ctx context.Context, provider Provider, profile Profile, assignment contract.ProvisionerAssignment, reason string, deleteResource bool) error {
	a := assignment.Assignment
	if a.State != worker.AssignmentPending {
		return nil
	}
	if _, err := r.coordinator.Abandon(ctx, a.ID, contract.AbandonProvisionerAssignmentRequest{ProviderError: reason}); err != nil {
		r.reconcileError("abandon", profile.ProviderID, profile.ProfileName)
		return fmt.Errorf("abandon pending assignment %s: %w", a.ID, err)
	}
	if !deleteResource {
		return nil
	}
	if err := provider.Delete(ctx, IdentityOf(assignment)); err != nil {
		r.cleanupError(profile)
		return fmt.Errorf("delete abandoned assignment %s: %w", a.ID, err)
	}
	if _, err := r.coordinator.Cleaned(ctx, a.ID); err != nil {
		r.cleanupError(profile)
		return fmt.Errorf("mark abandoned assignment %s cleaned: %w", a.ID, err)
	}
	return nil
}

func (r *Reconciler) rejectUnapprovedAssignment(ctx context.Context, profile Profile, assignment contract.ProvisionerAssignment, reason string) error {
	a := assignment.Assignment
	if _, err := r.coordinator.Abandon(ctx, a.ID, contract.AbandonProvisionerAssignmentRequest{ProviderError: reason}); err != nil {
		r.reconcileError("abandon", profile.ProviderID, profile.ProfileName)
		return fmt.Errorf("abandon unapproved assignment %s: %w", a.ID, err)
	}
	if _, err := r.coordinator.Cleaned(ctx, a.ID); err != nil {
		r.cleanupError(profile)
		return fmt.Errorf("mark unapproved assignment %s cleaned: %w", a.ID, err)
	}
	return fmt.Errorf("reject assignment %s: %s", a.ID, reason)
}

func (r *Reconciler) retryDelay(retryCount int) time.Duration {
	if retryCount < 0 {
		retryCount = 0
	}
	delay := r.retryBase
	for i := 0; i < retryCount && delay < r.retryMax; i++ {
		if delay > r.retryMax/2 {
			return r.retryMax
		}
		delay *= 2
	}
	if delay > r.retryMax {
		return r.retryMax
	}
	return delay
}

func randomProviderRequestID() (string, error) {
	var random [18]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return "pr_" + hex.EncodeToString(random[:]), nil
}

func (r *Reconciler) recoveryProvider(assignment contract.ProvisionerAssignment) (Profile, Provider, bool, error) {
	a := assignment.Assignment
	profile := Profile{
		ProviderID: a.ProviderID, ProfileName: a.ProfileName, ProviderType: a.ProviderType,
		ProviderOptions: a.ProviderOptions, Labels: a.ProfileLabels, Taints: a.ProfileTaints,
		HarnessModels: a.ProfileHarnessModels, AllowedRoles: a.AllowedRoles,
		AllowedBuckets: a.AllowedBuckets, RequiredSelector: a.RequiredSelector,
	}
	if configured, ok := r.profileByID[profileKey(a.ProviderID, a.ProfileName)]; ok && profileMatchesAssignment(configured, a) {
		provider := r.providers[configured.Provider]
		if provider == nil {
			return Profile{}, nil, false, fmt.Errorf("configured provider %q is unavailable", configured.Provider)
		}
		return configured, provider, true, nil
	}
	if r.providerFactory == nil {
		return Profile{}, nil, false, fmt.Errorf("profile %s/%s descriptor is not configured and provider recovery is unavailable", a.ProviderID, a.ProfileName)
	}
	cacheKey := "recovery:" + a.ProviderType + ":" + a.ProviderID + "/" + a.ProfileName
	if provider := r.providers[cacheKey]; provider != nil {
		profile.Provider = cacheKey
		return profile, provider, false, nil
	}
	provider, err := r.providerFactory(profile)
	if err != nil {
		return Profile{}, nil, false, fmt.Errorf("construct persisted %s provider: %w", a.ProviderType, err)
	}
	profile.Provider = cacheKey
	r.providers[cacheKey] = provider
	return profile, provider, false, nil
}

func profileMatchesAssignment(profile Profile, assignment worker.Assignment) bool {
	return profile.ProviderID == assignment.ProviderID &&
		profile.ProfileName == assignment.ProfileName &&
		profile.ProviderType == assignment.ProviderType &&
		equalStringMaps(profile.ProviderOptions, assignment.ProviderOptions) &&
		equalStringMaps(profile.Labels, assignment.ProfileLabels) &&
		equalEmptySlices(profile.Taints, assignment.ProfileTaints) &&
		equalHarnessModels(profile.HarnessModels, assignment.ProfileHarnessModels) &&
		equalEmptySlices(profile.AllowedRoles, assignment.AllowedRoles) &&
		equalEmptySlices(profile.AllowedBuckets, assignment.AllowedBuckets) &&
		equalStringMaps(profile.RequiredSelector, assignment.RequiredSelector)
}

func equalHarnessModels(left, right []flowharness.Model) bool {
	if len(left) != len(right) {
		return false
	}
	for _, expected := range left {
		if !worker.HarnessModelAvailable(right, expected.Harness, expected.QualifiedID) {
			return false
		}
	}
	return true
}

func equalStringMaps(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}

func equalEmptySlices[T any](left, right []T) bool {
	if len(left) == 0 && len(right) == 0 {
		return true
	}
	return reflect.DeepEqual(left, right)
}

func profileKey(providerID, profileName string) string { return providerID + "\x00" + profileName }

func metricLabels(profile Profile) map[string]string {
	return map[string]string{"provider": profile.ProviderID, "profile": profile.ProfileName}
}

func (r *Reconciler) reserveError(profile Profile) {
	if r.metrics.ReserveErrors != nil {
		r.metrics.ReserveErrors.Inc(metricLabels(profile))
	}
	r.reconcileError("reserve", profile.ProviderID, profile.ProfileName)
}
func (r *Reconciler) launchError(profile Profile) {
	if r.metrics.LaunchErrors != nil {
		r.metrics.LaunchErrors.Inc(metricLabels(profile))
	}
	r.reconcileError("launch", profile.ProviderID, profile.ProfileName)
}
func (r *Reconciler) cleanupError(profile Profile) {
	if r.metrics.CleanupErrors != nil {
		r.metrics.CleanupErrors.Inc(metricLabels(profile))
	}
	r.reconcileError("cleanup", profile.ProviderID, profile.ProfileName)
}
func (r *Reconciler) reconcileError(operation, providerID, profileName string) {
	if r.metrics.ReconcileErrors != nil {
		r.metrics.ReconcileErrors.Inc(map[string]string{"operation": operation, "provider": providerID, "profile": profileName})
	}
}

func (r *Reconciler) updateActiveMetrics(assignments []contract.ProvisionerAssignment) {
	if r.metrics.ActiveAssignments == nil {
		return
	}
	counts := make(map[string]float64)
	labelsByKey := make(map[string]map[string]string)
	for _, value := range assignments {
		a := value.Assignment
		if a.State == worker.AssignmentClosed {
			continue
		}
		labels := map[string]string{"provider": a.ProviderID, "profile": a.ProfileName, "state": string(a.State)}
		key := a.ProviderID + "\x00" + a.ProfileName + "\x00" + string(a.State)
		counts[key]++
		labelsByKey[key] = labels
	}
	for key, labels := range r.activeSeries {
		if _, exists := counts[key]; !exists {
			r.metrics.ActiveAssignments.Set(0, labels)
		}
	}
	for key, count := range counts {
		r.metrics.ActiveAssignments.Set(count, labelsByKey[key])
	}
	r.activeSeries = labelsByKey
}
