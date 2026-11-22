// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	netboxv1alpha1 "go.miloapis.com/netbox-provider/api/v1alpha1"
	"go.miloapis.com/netbox-provider/internal/fleet"
	"go.miloapis.com/netbox-provider/internal/netbox"
	"go.miloapis.com/netbox-provider/internal/plan"
)

// Condition types. Ready is the summary; it describes this import, never the
// readiness of any Fleet record or hardware.
const (
	ConditionConfigurationResolved = "ConfigurationResolved"
	ConditionSourceObserved        = "SourceObserved"
	ConditionFleetObserved         = "FleetObserved"
	ConditionApplied               = "Applied"
	ConditionReady                 = "Ready"
)

// Condition reasons.
const (
	ReasonResolved             = "Resolved"
	ReasonMappingNotFound      = "MappingNotFound"
	ReasonSecretNotFound       = "SecretNotFound"
	ReasonSecretKeyMissing     = "SecretKeyMissing"
	ReasonTokenInvalid         = "TokenInvalid"
	ReasonTokenRejected        = "TokenRejected"
	ReasonInvalidConfiguration = "InvalidConfiguration"
	ReasonObserved             = "Observed"
	ReasonReadFailed           = "ReadFailed"
	ReasonNotAttempted         = "NotAttempted"
	ReasonObserveOnly          = "ObserveOnly"
	ReasonApplying             = "Applying"
	ReasonApplied              = "Applied"
	ReasonApplyUncertain       = "ApplyUncertain"
	ReasonReviewRequired       = "ReviewRequired"
	ReasonSuspended            = "Suspended"
)

const (
	mappingIndex = "spec.mappingRef.name"
	// pollInterval re-checks inputs that are not watched, such as Secrets.
	pollInterval = time.Minute
	// defaultInterval matches the CRD default for spec.interval.
	defaultInterval = 30 * time.Minute
	// rejectedCredentialsInterval is the shortest wait before retrying a token
	// NetBox rejected, so a bad token cannot trip address bans.
	rejectedCredentialsInterval = time.Hour
	// failureBaseDelay and failureMaxDelay bound retries of other failures.
	failureBaseDelay = 30 * time.Second
	failureMaxDelay  = 30 * time.Minute
	// maxStatusRecords bounds status.records.
	maxStatusRecords = 100
)

// NetBoxSourceReconciler reconciles NetBoxSource objects.
//
// It reads NetBoxSource, NetBoxMapping and the token Secret through Client,
// and reads and creates Fleet inventory records through Fleet. Fleet must be
// an uncached client so plans are made against current state. The reconciler
// never calls Update, Patch or Delete on a Fleet record.
type NetBoxSourceReconciler struct {
	Client client.Client
	// APIReader reads Secrets without caching them cluster-wide.
	APIReader client.Reader
	Fleet     client.Client
	// HTTPClient is the NetBox transport; nil uses the default transport.
	HTTPClient *http.Client
	// Now is the clock; nil uses time.Now.
	Now func() time.Time
	// NewReader builds the NetBox reader; nil uses netbox.New.
	NewReader func(netbox.Config) (netbox.Reader, error)
}

// Access is declared by hand, not by kubebuilder RBAC markers, following
// Fleet: config/base/manager grants namespaced access to NetBoxSources,
// NetBoxMappings and Secrets on the local cluster, and Fleet inventory access
// is Milo IAM (docs/architecture/fleet-contract.md#4-iam), never local RBAC.

// Reconcile observes NetBox and Fleet and, only in Create mode, creates new
// records. It returns an error only when a retry can fix it; see "Reconcile
// errors" in CLAUDE.md.
func (r *NetBoxSourceReconciler) Reconcile(ctx context.Context, req reconcile.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var source netboxv1alpha1.NetBoxSource
	if err := r.Client.Get(ctx, req.NamespacedName, &source); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	status := &source.Status
	status.ObservedGeneration = source.Generation
	set := func(conditionType string, s metav1.ConditionStatus, reason, message string) {
		apimeta.SetStatusCondition(&status.Conditions, metav1.Condition{
			Type: conditionType, Status: s, Reason: reason, Message: message, ObservedGeneration: source.Generation,
		})
	}

	if source.Spec.Suspend {
		set(ConditionReady, metav1.ConditionFalse, ReasonSuspended, "reads and writes are suspended")
		return ctrl.Result{}, r.updateStatus(ctx, &source)
	}

	// Resolve configuration. Nothing is read from NetBox until it resolves.
	mapping, token, reason, message, err := r.resolve(ctx, &source)
	if err != nil {
		return ctrl.Result{}, err
	}
	if reason != "" {
		set(ConditionConfigurationResolved, metav1.ConditionFalse, reason, message)
		set(ConditionSourceObserved, metav1.ConditionUnknown, ReasonNotAttempted, "configuration is unresolved")
		set(ConditionFleetObserved, metav1.ConditionUnknown, ReasonNotAttempted, "configuration is unresolved")
		set(ConditionApplied, metav1.ConditionUnknown, ReasonNotAttempted, "configuration is unresolved")
		set(ConditionReady, metav1.ConditionFalse, reason, message)
		return ctrl.Result{RequeueAfter: pollInterval}, r.updateStatus(ctx, &source)
	}
	set(ConditionConfigurationResolved, metav1.ConditionTrue, ReasonResolved, "mapping and token resolved")

	// Read NetBox. A failed or partial read is Unknown; nothing is planned
	// or written from it.
	reader, err := r.newReader(source.Spec, token)
	if err != nil {
		set(ConditionConfigurationResolved, metav1.ConditionFalse, ReasonInvalidConfiguration, err.Error())
		set(ConditionReady, metav1.ConditionFalse, ReasonInvalidConfiguration, err.Error())
		if updateErr := r.updateStatus(ctx, &source); updateErr != nil {
			return ctrl.Result{}, updateErr
		}
		return ctrl.Result{}, reconcile.TerminalError(err)
	}
	snapshot, err := reader.Snapshot(ctx, netbox.Scope{SiteIDs: source.Spec.Scope.SiteIDs}, r.now())
	if errors.Is(err, netbox.ErrCredentialsRejected) {
		retry := max(rejectedCredentialsInterval, interval(source))
		message := fmt.Sprintf("NetBox rejected the token; it is not retried for %s: %v", retry, err)
		set(ConditionConfigurationResolved, metav1.ConditionFalse, ReasonTokenRejected, message)
		set(ConditionSourceObserved, metav1.ConditionUnknown, ReasonReadFailed, "no complete NetBox observation")
		set(ConditionApplied, metav1.ConditionUnknown, ReasonNotAttempted, "no complete NetBox observation")
		set(ConditionReady, metav1.ConditionFalse, ReasonTokenRejected, message)
		return ctrl.Result{RequeueAfter: retry}, r.updateStatus(ctx, &source)
	}
	if err != nil {
		message := "NetBox read failed; source state is unknown: " + err.Error()
		set(ConditionSourceObserved, metav1.ConditionUnknown, ReasonReadFailed, message)
		set(ConditionApplied, metav1.ConditionUnknown, ReasonNotAttempted, "no complete NetBox observation")
		set(ConditionReady, metav1.ConditionUnknown, ReasonReadFailed, message)
		if updateErr := r.updateStatus(ctx, &source); updateErr != nil {
			return ctrl.Result{}, updateErr
		}
		return ctrl.Result{}, fmt.Errorf("reading NetBox: %w", err)
	}
	set(ConditionSourceObserved, metav1.ConditionTrue, ReasonObserved, "complete read of the scoped collections")

	// Read Fleet. Every kind must list completely before a plan is made.
	existing, err := r.observeFleet(ctx, source.Spec.Target.Namespace)
	if err != nil {
		message := "Fleet read failed; existing state is unknown: " + err.Error()
		set(ConditionFleetObserved, metav1.ConditionUnknown, ReasonReadFailed, message)
		set(ConditionApplied, metav1.ConditionUnknown, ReasonNotAttempted, "no complete Fleet observation")
		set(ConditionReady, metav1.ConditionUnknown, ReasonReadFailed, message)
		if updateErr := r.updateStatus(ctx, &source); updateErr != nil {
			return ctrl.Result{}, updateErr
		}
		return ctrl.Result{}, fmt.Errorf("reading Fleet: %w", err)
	}
	set(ConditionFleetObserved, metav1.ConditionTrue, ReasonObserved, "listed existing inventory in the target namespace")

	p := plan.Build(plan.Input{
		System:    source.Spec.SourceID,
		Namespace: source.Spec.Target.Namespace,
		Snapshot:  snapshot,
		Mapping:   mapping.Spec,
		Existing:  existing,
	})
	status.LastObservation = &netboxv1alpha1.ObservationSummary{
		SnapshotDigest:  snapshot.Digest(),
		ObservedAt:      metav1.NewTime(snapshot.ObservedAt),
		MappingRevision: mapping.Spec.Revision,
		Counts:          snapshot.Counts(),
	}

	created, updated, uncertain := 0, 0, 0
	requeue := interval(source)
	creates, updates := p.Count(plan.ActionCreate), p.Count(plan.ActionUpdate)
	switch {
	case source.Spec.Mode != netboxv1alpha1.SourceModeCreate:
		set(ConditionApplied, metav1.ConditionFalse, ReasonObserveOnly,
			fmt.Sprintf("%d records would be created and %d updated; set spec.mode to Create to opt in to writes", creates, updates))
	case creates+updates == 0:
		set(ConditionApplied, metav1.ConditionTrue, ReasonApplied, "no records to write")
	default:
		// Retain the exact inputs before any effect.
		set(ConditionApplied, metav1.ConditionUnknown, ReasonApplying,
			fmt.Sprintf("applying %d new and %d changed records from snapshot %s and mapping revision %s", creates, updates, status.LastObservation.SnapshotDigest, mapping.Spec.Revision))
		r.summarize(&source, p)
		if err := r.updateStatus(ctx, &source); err != nil {
			return ctrl.Result{}, err
		}
		created, updated, uncertain = r.apply(ctx, p)
		if uncertain > 0 {
			set(ConditionApplied, metav1.ConditionUnknown, ReasonApplyUncertain,
				fmt.Sprintf("%d writes have an unknown outcome; they are re-observed before any retry", uncertain))
			requeue = pollInterval
		} else {
			set(ConditionApplied, metav1.ConditionTrue, ReasonApplied, fmt.Sprintf("created %d and updated %d records", created, updated))
		}
	}

	r.summarize(&source, p)
	logPlan(logger, p)
	blocked := status.Plan.Quarantined + status.Plan.Unresolved
	switch {
	case uncertain > 0:
		set(ConditionReady, metav1.ConditionUnknown, ReasonApplyUncertain, "some writes have an unknown outcome")
	case blocked > 0:
		set(ConditionReady, metav1.ConditionFalse, ReasonReviewRequired,
			fmt.Sprintf("%d quarantined and %d unresolved records need review; see status.records", status.Plan.Quarantined, status.Plan.Unresolved))
	default:
		set(ConditionReady, metav1.ConditionTrue, ReasonObserved, "complete observation with no conflicts")
	}
	logger.Info("reconciled NetBox source", "mode", source.Spec.Mode, "snapshot", status.LastObservation.SnapshotDigest,
		"create", status.Plan.Create, "update", status.Plan.Update, "created", created, "updated", updated,
		"uncertain", uncertain, "quarantined", status.Plan.Quarantined)
	return ctrl.Result{RequeueAfter: requeue}, r.updateStatus(ctx, &source)
}

// interval is the source's re-read period.
func interval(source netboxv1alpha1.NetBoxSource) time.Duration {
	if source.Spec.Interval != nil {
		return source.Spec.Interval.Duration
	}
	return defaultInterval
}

func (r *NetBoxSourceReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// resolve reads the mapping and token. A non-empty reason is a condition the
// user must fix; err is a transient read failure.
func (r *NetBoxSourceReconciler) resolve(ctx context.Context, source *netboxv1alpha1.NetBoxSource) (*netboxv1alpha1.NetBoxMapping, string, string, string, error) {
	var mapping netboxv1alpha1.NetBoxMapping
	key := types.NamespacedName{Namespace: source.Namespace, Name: source.Spec.MappingRef.Name}
	if err := r.Client.Get(ctx, key, &mapping); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, "", ReasonMappingNotFound, fmt.Sprintf("NetBoxMapping %q not found", key.Name), nil
		}
		return nil, "", "", "", err
	}
	var secret corev1.Secret
	ref := source.Spec.TokenSecretRef
	if err := r.APIReader.Get(ctx, types.NamespacedName{Namespace: source.Namespace, Name: ref.Name}, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, "", ReasonSecretNotFound, fmt.Sprintf("Secret %q not found", ref.Name), nil
		}
		return nil, "", "", "", err
	}
	keyName := ref.Key
	if keyName == "" {
		keyName = "token"
	}
	value, ok := secret.Data[keyName]
	if !ok {
		return nil, "", ReasonSecretKeyMissing, fmt.Sprintf("Secret %q has no key %q", ref.Name, keyName), nil
	}
	token := strings.TrimSpace(string(value))
	if token == "" || strings.ContainsAny(token, " \t\r\n") {
		return nil, "", ReasonTokenInvalid, fmt.Sprintf("Secret %q key %q is empty or contains whitespace", ref.Name, keyName), nil
	}
	return &mapping, token, "", "", nil
}

func (r *NetBoxSourceReconciler) newReader(spec netboxv1alpha1.NetBoxSourceSpec, token string) (netbox.Reader, error) {
	scheme := string(spec.AuthScheme)
	if scheme == "" {
		scheme = string(netboxv1alpha1.AuthSchemeBearer)
	}
	cfg := netbox.Config{BaseURL: spec.URL, Token: token, Scheme: scheme, HTTPClient: r.HTTPClient, Limits: readLimits(spec.ReadLimits)}
	if r.NewReader != nil {
		return r.NewReader(cfg)
	}
	return netbox.New(cfg)
}

// readLimits applies a source's overrides to the reader's defaults.
func readLimits(l *netboxv1alpha1.ReadLimits) netbox.Limits {
	limits := netbox.DefaultLimits
	if l == nil {
		return limits
	}
	if l.MaxRecordsPerCollection > 0 {
		limits.Records = int(l.MaxRecordsPerCollection)
		limits.Pages = netbox.PagesFor(limits.Records)
	}
	if l.MaxMiBPerCollection > 0 {
		limits.CollectionBytes = int64(l.MaxMiBPerCollection) << 20
	}
	if l.CollectionTimeout != nil {
		limits.CollectionTimeout = l.CollectionTimeout.Duration
	}
	return limits
}

func (r *NetBoxSourceReconciler) observeFleet(ctx context.Context, namespace string) (plan.Existing, error) {
	existing := plan.Existing{}
	for _, kind := range fleet.ReadKinds {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(fleet.ListGVK(kind))
		if err := r.Fleet.List(ctx, list, client.InNamespace(namespace)); err != nil {
			return nil, fmt.Errorf("listing %s: %s", kind, truncate(apiMessage(err), 200))
		}
		existing[kind] = list.Items
	}
	return existing, nil
}

// apply writes planned records in dependency order. Every write is a
// server-side apply under plan.FieldManager, never forced: a create for a
// missing record, and an update, preconditioned on the observed UID, for a
// record this controller applied before. A field another manager owns is a
// conflict, which quarantines the record instead of taking the field. A write
// that fails ambiguously is counted uncertain; its dependents are skipped
// until a later reconcile re-observes Fleet.
func (r *NetBoxSourceReconciler) apply(ctx context.Context, p *plan.Plan) (int, int, int) {
	logger := log.FromContext(ctx)
	created, updated, uncertain := 0, 0, 0
	// blocked maps a target key to true when its write outcome is unknown
	// and false when it definitely was not written.
	blocked := map[string]bool{}
	for _, kind := range fleet.WriteKinds {
		for _, c := range p.Candidates {
			if c.Kind != kind || (c.Action != plan.ActionCreate && c.Action != plan.ActionUpdate) {
				continue
			}
			depUnknown, depBlocked := false, false
			for _, dep := range c.DependsOn {
				if unknown, ok := blocked[dep]; ok {
					depBlocked = true
					depUnknown = depUnknown || unknown
				}
			}
			if depUnknown {
				blocked[c.Key()] = true
				c.Message = "a dependency's write outcome is unknown; retried after re-observation"
				uncertain++
				continue
			}
			if depBlocked {
				blocked[c.Key()] = false
				c.Action, c.Reason, c.Message = plan.ActionUnresolved, plan.ReasonDependencyBlocked, "a dependency was not written"
				continue
			}
			creating := c.Action == plan.ActionCreate
			obj := c.Object.DeepCopy()
			if !creating {
				obj.SetUID(c.ExistingUID)
			}
			err := r.Fleet.Apply(ctx, client.ApplyConfigurationFromUnstructured(obj), client.FieldOwner(plan.FieldManager))
			switch {
			case err == nil && creating && appliedOver(obj):
				// Another writer created this name between observation and
				// apply. The record is reported, never treated as ours.
				c.Action, c.Reason = plan.ActionQuarantined, plan.ReasonNameConflict
				c.Message = "another writer created this record while it was being applied; review it, it is not this controller's"
				blocked[c.Key()] = false
				logger.Info("applied over a record another writer created", "kind", kind, "name", c.Name)
			case err == nil && creating:
				c.Action, c.Reason, c.Message = actionCreated, "Created", ""
				created++
			case err == nil:
				c.Action, c.Reason, c.Message = actionUpdated, "Updated", ""
				updated++
			case isFieldManagerConflict(err):
				c.Action, c.Reason, c.Message = plan.ActionQuarantined, plan.ReasonApplyConflict, truncate(conflictMessage(err), 256)
				blocked[c.Key()] = false
				logger.Info("apply conflict; ownership is never forced", "kind", kind, "name", c.Name, "conflicts", conflictMessage(err))
			case apierrors.IsInvalid(err) || apierrors.IsForbidden(err) || apierrors.IsBadRequest(err):
				// A definite rejection: nothing was written.
				reason := "Rejected"
				if strings.Contains(apiMessage(err), "immutable") {
					reason = plan.ReasonImmutableChanged
				}
				c.Action, c.Reason, c.Message = plan.ActionQuarantined, reason, truncate(apiMessage(err), 256)
				blocked[c.Key()] = false
			default:
				blocked[c.Key()] = true
				c.Message = "write outcome unknown: " + truncate(apiMessage(err), 200)
				uncertain++
				logger.Info("write outcome unknown", "kind", kind, "name", c.Name)
			}
		}
	}
	return created, updated, uncertain
}

// appliedOver reports whether a create apply landed on a record that other
// managers had already written.
func appliedOver(obj *unstructured.Unstructured) bool {
	for _, entry := range obj.GetManagedFields() {
		if entry.Manager != plan.FieldManager {
			return true
		}
	}
	return false
}

func isFieldManagerConflict(err error) bool {
	var status apierrors.APIStatus
	if !apierrors.IsConflict(err) || !errors.As(err, &status) || status.Status().Details == nil {
		return false
	}
	for _, cause := range status.Status().Details.Causes {
		if cause.Type == metav1.CauseTypeFieldManagerConflict {
			return true
		}
	}
	return false
}

// conflictMessage names each conflicting manager and field path.
func conflictMessage(err error) string {
	var status apierrors.APIStatus
	if !errors.As(err, &status) || status.Status().Details == nil {
		return "apply conflict"
	}
	var parts []string
	for _, cause := range status.Status().Details.Causes {
		if cause.Type == metav1.CauseTypeFieldManagerConflict {
			parts = append(parts, cause.Field+" ("+cause.Message+")")
		}
	}
	return "fields owned by another manager; ownership is never forced: " + strings.Join(parts, "; ")
}

// actionCreated and actionUpdated mark candidates written in this reconcile.
const (
	actionCreated plan.Action = "Created"
	actionUpdated plan.Action = "Updated"
)

func apiMessage(err error) string {
	var status apierrors.APIStatus
	if errors.As(err, &status) {
		return status.Status().Message
	}
	return "request failed"
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// logPlan logs every planned target and source record. Status holds at most
// maxStatusRecords rows; the log is where a whole plan can be reviewed. It
// carries the same fields as status, never serials or tokens.
func logPlan(logger logr.Logger, p *plan.Plan) {
	for _, c := range p.Candidates {
		logger.Info("planned record", "source", c.Source, "target", c.Key(), "action", c.Action, "reason", c.Reason, "message", c.Message)
	}
	for _, rec := range append(append([]plan.Record{}, p.Records...), p.Retained...) {
		logger.Info("planned record", "source", rec.Source, "action", rec.State, "reason", rec.Reason, "message", truncate(rec.Message, 256))
	}
}

// summarize writes plan counts and the bounded record list to status.
func (r *NetBoxSourceReconciler) summarize(source *netboxv1alpha1.NetBoxSource, p *plan.Plan) {
	summary := &netboxv1alpha1.PlanSummary{}
	var rows []netboxv1alpha1.RecordStatus
	for _, c := range p.Candidates {
		row := netboxv1alpha1.RecordStatus{Source: c.Source, Target: c.Key(), Reason: c.Reason, Message: c.Message}
		switch c.Action {
		case plan.ActionCreate:
			summary.Create++
			row.State = netboxv1alpha1.RecordStateCreate
		case actionCreated:
			summary.Created++
			row.State = netboxv1alpha1.RecordStateCreated
		case plan.ActionUpdate:
			summary.Update++
			row.State = netboxv1alpha1.RecordStateUpdate
		case actionUpdated:
			summary.Updated++
			row.State = netboxv1alpha1.RecordStateUpdated
		case plan.ActionUnchanged:
			summary.Unchanged++
			row.State = netboxv1alpha1.RecordStateUnchanged
		case plan.ActionQuarantined:
			summary.Quarantined++
			row.State = netboxv1alpha1.RecordStateQuarantined
		default:
			summary.Unresolved++
			row.State = netboxv1alpha1.RecordStateUnresolved
		}
		rows = append(rows, row)
	}
	for _, rec := range append(append([]plan.Record{}, p.Records...), p.Retained...) {
		switch rec.State {
		case netboxv1alpha1.RecordStateExcluded:
			summary.Excluded++
		case netboxv1alpha1.RecordStateUnsupported:
			summary.Unsupported++
		case netboxv1alpha1.RecordStateRetainedMissingSource:
			summary.RetainedMissingSource++
		default:
			summary.Unresolved++
		}
		rows = append(rows, netboxv1alpha1.RecordStatus{Source: rec.Source, State: rec.State, Reason: rec.Reason, Message: truncate(rec.Message, 256)})
	}
	priority := map[netboxv1alpha1.RecordState]int{
		netboxv1alpha1.RecordStateQuarantined: 0, netboxv1alpha1.RecordStateUnresolved: 1,
		netboxv1alpha1.RecordStateUnsupported: 2, netboxv1alpha1.RecordStateRetainedMissingSource: 3,
		netboxv1alpha1.RecordStateCreate: 4, netboxv1alpha1.RecordStateUpdate: 4,
		netboxv1alpha1.RecordStateCreated: 5, netboxv1alpha1.RecordStateUpdated: 5,
		netboxv1alpha1.RecordStateExcluded: 6, netboxv1alpha1.RecordStateUnchanged: 7,
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if priority[rows[i].State] != priority[rows[j].State] {
			return priority[rows[i].State] < priority[rows[j].State]
		}
		return rows[i].Target+rows[i].Source < rows[j].Target+rows[j].Source
	})
	source.Status.RecordsTruncated = len(rows) > maxStatusRecords
	if len(rows) > maxStatusRecords {
		rows = rows[:maxStatusRecords]
	}
	source.Status.Plan = summary
	source.Status.Records = rows
}

func (r *NetBoxSourceReconciler) updateStatus(ctx context.Context, source *netboxv1alpha1.NetBoxSource) error {
	if err := r.Client.Status().Update(ctx, source); err != nil {
		return fmt.Errorf("updating status: %w", err)
	}
	return nil
}

// SetupWithManager sets up the controller with the Manager. It watches
// NetBoxMappings and requeues the sources that reference them. Secrets are
// polled, not watched, so Secrets are never cached cluster-wide.
func (r *NetBoxSourceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &netboxv1alpha1.NetBoxSource{}, mappingIndex,
		func(obj client.Object) []string {
			return []string{obj.(*netboxv1alpha1.NetBoxSource).Spec.MappingRef.Name}
		}); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("netboxsource").
		// Status writes do not trigger reads; only spec changes, mappings and
		// requeues do.
		For(&netboxv1alpha1.NetBoxSource{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&netboxv1alpha1.NetBoxMapping{}, handler.EnqueueRequestsFromMapFunc(r.sourcesForMapping)).
		WithOptions(controller.Options{
			RateLimiter: workqueue.NewTypedItemExponentialFailureRateLimiter[reconcile.Request](failureBaseDelay, failureMaxDelay),
		}).
		Complete(r)
}

func (r *NetBoxSourceReconciler) sourcesForMapping(ctx context.Context, obj client.Object) []reconcile.Request {
	var sources netboxv1alpha1.NetBoxSourceList
	if err := r.Client.List(ctx, &sources, client.InNamespace(obj.GetNamespace()), client.MatchingFields{mappingIndex: obj.GetName()}); err != nil {
		return nil
	}
	requests := make([]reconcile.Request, 0, len(sources.Items))
	for _, s := range sources.Items {
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&s)})
	}
	return requests
}
