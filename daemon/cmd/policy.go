// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	stdlog "log"
	"net/netip"
	"sync"
	"time"

	"github.com/go-openapi/runtime/middleware"
	"github.com/google/uuid"

	"github.com/cilium/cilium/api/v1/models"
	. "github.com/cilium/cilium/api/v1/server/restapi/policy"
	"github.com/cilium/cilium/pkg/api"
	"github.com/cilium/cilium/pkg/auth"
	authMonitor "github.com/cilium/cilium/pkg/auth/monitor"
	"github.com/cilium/cilium/pkg/crypto/certificatemanager"
	"github.com/cilium/cilium/pkg/endpoint"
	"github.com/cilium/cilium/pkg/endpoint/regeneration"
	"github.com/cilium/cilium/pkg/endpointmanager"
	"github.com/cilium/cilium/pkg/envoy"
	"github.com/cilium/cilium/pkg/eventqueue"
	"github.com/cilium/cilium/pkg/identity"
	"github.com/cilium/cilium/pkg/identity/cache"
	"github.com/cilium/cilium/pkg/labels"
	"github.com/cilium/cilium/pkg/logging/logfields"
	bpfIPCache "github.com/cilium/cilium/pkg/maps/ipcache"
	"github.com/cilium/cilium/pkg/metrics"
	monitorAPI "github.com/cilium/cilium/pkg/monitor/api"
	"github.com/cilium/cilium/pkg/option"
	"github.com/cilium/cilium/pkg/policy"
	policyAPI "github.com/cilium/cilium/pkg/policy/api"
	"github.com/cilium/cilium/pkg/safetime"
	"github.com/cilium/cilium/pkg/trigger"
)

// initPolicy initializes the core policy components of the daemon.
func (d *Daemon) initPolicy(epMgr endpointmanager.EndpointManager) error {
	// Reuse policy.TriggerMetrics and PolicyTriggerInterval here since
	// this is only triggered by agent configuration changes for now and
	// should be counted in pol.TriggerMetrics.
	rt, err := trigger.NewTrigger(trigger.Parameters{
		Name:            "datapath-regeneration",
		MetricsObserver: &policy.TriggerMetrics{},
		MinInterval:     option.Config.PolicyTriggerInterval,
		TriggerFunc:     d.datapathRegen,
	})
	if err != nil {
		return fmt.Errorf("failed to create datapath regeneration trigger: %w", err)
	}
	d.datapathRegenTrigger = rt

	d.policy = policy.NewPolicyRepository(d.identityAllocator,
		d.identityAllocator.GetIdentityCache(),
		certificatemanager.NewManager(option.Config.CertDirectory, d.clientset))
	d.policy.SetEnvoyRulesFunc(envoy.GetEnvoyHTTPRules)
	d.policyUpdater, err = policy.NewUpdater(d.policy, epMgr)
	if err != nil {
		return fmt.Errorf("failed to create policy update trigger: %w", err)
	}

	d.monitorAgent.RegisterNewConsumer(authMonitor.AddAuthManager(auth.NewAuthManager(epMgr)))

	return nil
}

// TriggerPolicyUpdates triggers policy updates by deferring to the
// policy.Updater to handle them.
func (d *Daemon) TriggerPolicyUpdates(force bool, reason string) {
	d.policyUpdater.TriggerPolicyUpdates(force, reason)
}

// UpdateIdentities informs the policy package of all identity changes
// and also triggers policy updates.
//
// The caller is responsible for making sure the same identity is not
// present in both 'added' and 'deleted'.
func (d *Daemon) UpdateIdentities(added, deleted cache.IdentityCache) {
	wg := &sync.WaitGroup{}
	d.policy.GetSelectorCache().UpdateIdentities(added, deleted, wg)
	// Wait for update propagation to endpoints before triggering policy updates
	wg.Wait()
	d.TriggerPolicyUpdates(false, "one or more identities created or deleted")
}

type getPolicyResolve struct {
	daemon *Daemon
}

func NewGetPolicyResolveHandler(d *Daemon) GetPolicyResolveHandler {
	return &getPolicyResolve{daemon: d}
}

func (h *getPolicyResolve) Handle(params GetPolicyResolveParams) middleware.Responder {
	log.WithField(logfields.Params, logfields.Repr(params)).Debug("GET /policy/resolve request")

	d := h.daemon

	var policyEnforcementMsg string
	isPolicyEnforcementEnabled := true
	fromEgress := true
	toIngress := true
	d.policy.Mutex.RLock()

	// If policy enforcement isn't enabled, then traffic is allowed.
	if policy.GetPolicyEnabled() == option.NeverEnforce {
		policyEnforcementMsg = "Policy enforcement is disabled for the daemon."
		isPolicyEnforcementEnabled = false
	} else if policy.GetPolicyEnabled() == option.DefaultEnforcement {
		// If there are no rules matching the set of from / to labels provided in
		// the API request, that means that policy enforcement is not enabled
		// for the endpoints corresponding to said sets of labels; thus, we allow
		// traffic between these sets of labels, and do not enforce policy between them.
		_, fromEgress = d.policy.GetRulesMatching(labels.NewSelectLabelArrayFromModel(params.TraceSelector.From.Labels))
		toIngress, _ = d.policy.GetRulesMatching(labels.NewSelectLabelArrayFromModel(params.TraceSelector.To.Labels))
		if !fromEgress && !toIngress {
			policyEnforcementMsg = "Policy enforcement is disabled because " +
				"no rules in the policy repository match any endpoint selector " +
				"from the provided destination sets of labels."
			isPolicyEnforcementEnabled = false
		}
	}

	d.policy.Mutex.RUnlock()

	// Return allowed verdict if policy enforcement isn't enabled between the two sets of labels.
	if !isPolicyEnforcementEnabled {
		buffer := new(bytes.Buffer)
		ctx := params.TraceSelector
		searchCtx := policy.SearchContext{
			From:    labels.NewSelectLabelArrayFromModel(ctx.From.Labels),
			Trace:   policy.TRACE_ENABLED,
			To:      labels.NewSelectLabelArrayFromModel(ctx.To.Labels),
			DPorts:  ctx.To.Dports,
			Logging: stdlog.New(buffer, "", 0),
		}
		if ctx.Verbose {
			searchCtx.Trace = policy.TRACE_VERBOSE
		}
		verdict := policyAPI.Allowed.String()
		searchCtx.PolicyTrace("Label verdict: %s\n", verdict)
		msg := fmt.Sprintf("%s\n  %s\n%s", searchCtx.String(), policyEnforcementMsg, buffer.String())
		return NewGetPolicyResolveOK().WithPayload(&models.PolicyTraceResult{
			Log:     msg,
			Verdict: verdict,
		})
	}

	// If we hit the following code, policy enforcement is enabled for at least
	// one of the endpoints corresponding to the provided sets of labels, or for
	// the daemon.
	ingressBuffer := new(bytes.Buffer)

	ctx := params.TraceSelector
	ingressSearchCtx := policy.SearchContext{
		Trace:   policy.TRACE_ENABLED,
		Logging: stdlog.New(ingressBuffer, "", 0),
		From:    labels.NewSelectLabelArrayFromModel(ctx.From.Labels),
		To:      labels.NewSelectLabelArrayFromModel(ctx.To.Labels),
		DPorts:  ctx.To.Dports,
	}
	if ctx.Verbose {
		ingressSearchCtx.Trace = policy.TRACE_VERBOSE
	}

	egressBuffer := new(bytes.Buffer)
	egressSearchCtx := ingressSearchCtx
	egressSearchCtx.Logging = stdlog.New(egressBuffer, "", 0)

	ingressVerdict := policyAPI.Allowed
	egressVerdict := policyAPI.Allowed
	d.policy.Mutex.RLock()
	if fromEgress {
		egressVerdict = d.policy.AllowsEgressRLocked(&egressSearchCtx)
	}
	if toIngress {
		ingressVerdict = d.policy.AllowsIngressRLocked(&ingressSearchCtx)
	}
	d.policy.Mutex.RUnlock()

	result := models.PolicyTraceResult{
		Log: egressBuffer.String() + "\n" + ingressBuffer.String(),
	}
	if ingressVerdict == policyAPI.Allowed && egressVerdict == policyAPI.Allowed {
		result.Verdict = policyAPI.Allowed.String()
	} else {
		result.Verdict = policyAPI.Denied.String()
	}

	return NewGetPolicyResolveOK().WithPayload(&result)
}

// PolicyAddEvent is a wrapper around the parameters for policyAdd.
type PolicyAddEvent struct {
	rules policyAPI.Rules
	opts  *policy.AddOptions
	d     *Daemon
}

// Handle implements pkg/eventqueue/EventHandler interface.
func (p *PolicyAddEvent) Handle(res chan interface{}) {
	p.d.policyAdd(p.rules, p.opts, res)
}

// PolicyAddResult is a wrapper around the values returned by policyAdd. It
// contains the new revision of a policy repository after adding a list of rules
// to it, and any error associated with adding rules to said repository.
type PolicyAddResult struct {
	newRev uint64
	err    error
}

// PolicyAdd adds a slice of rules to the policy repository owned by the
// daemon. Eventual changes in policy rules are propagated to all locally
// managed endpoints. Returns the policy revision number of the repository after
// adding the rules into the repository, or an error if the updated policy
// was not able to be imported.
func (d *Daemon) PolicyAdd(rules policyAPI.Rules, opts *policy.AddOptions) (newRev uint64, err error) {
	p := &PolicyAddEvent{
		rules: rules,
		opts:  opts,
		d:     d,
	}
	polAddEvent := eventqueue.NewEvent(p)
	resChan, err := d.policy.RepositoryChangeQueue.Enqueue(polAddEvent)
	if err != nil {
		return 0, fmt.Errorf("enqueue of PolicyAddEvent failed: %s", err)
	}

	res, ok := <-resChan
	if ok {
		pRes := res.(*PolicyAddResult)
		return pRes.newRev, pRes.err
	}
	return 0, fmt.Errorf("policy addition event was cancelled")
}

// policyAdd adds a slice of rules to the policy repository owned by the
// daemon. Eventual changes in policy rules are propagated to all locally
// managed endpoints. Returns the policy revision number of the repository after
// adding the rules into the repository, or an error if the updated policy
// was not able to be imported.
func (d *Daemon) policyAdd(sourceRules policyAPI.Rules, opts *policy.AddOptions, resChan chan interface{}) {
	policyAddStartTime := time.Now()
	if opts != nil && !opts.ProcessingStartTime.IsZero() {
		policyAddStartTime = opts.ProcessingStartTime
	}
	logger := log.WithField("policyAddRequest", uuid.New().String())

	if opts != nil && opts.Generated {
		logger.WithField(logfields.CiliumNetworkPolicy, sourceRules.String()).Debug("Policy Add Request")
	} else {
		logger.WithField(logfields.CiliumNetworkPolicy, sourceRules.String()).Info("Policy Add Request")
	}

	prefixes := policy.GetCIDRPrefixes(sourceRules)
	logger.WithField("prefixes", prefixes).Debug("Policy imported via API, found CIDR prefixes...")

	newPrefixLengths, err := d.prefixLengths.Add(prefixes)
	if err != nil {
		logger.WithError(err).WithField("prefixes", prefixes).Warn(
			"Failed to reference-count prefix lengths in CIDR policy")
		resChan <- &PolicyAddResult{
			newRev: 0,
			err:    api.Error(PutPolicyFailureCode, err),
		}
		return
	}
	if newPrefixLengths && !bpfIPCache.BackedByLPM() {
		// Only recompile if configuration has changed.
		logger.Debug("CIDR policy has changed; recompiling base programs")
		if err := d.Datapath().Loader().Reinitialize(d.ctx, d, d.mtuConfig.GetDeviceMTU(), d.Datapath(), d.l7Proxy); err != nil {
			_ = d.prefixLengths.Delete(prefixes)
			err2 := fmt.Errorf("Unable to recompile base programs: %s", err)
			logger.WithError(err2).WithField("prefixes", prefixes).Warn(
				"Failed to recompile base programs due to prefix length count change")
			resChan <- &PolicyAddResult{
				newRev: 0,
				err:    api.Error(PutPolicyFailureCode, err),
			}
			return
		}
	}

	// Any newly allocated identities MUST be upserted to the ipcache if
	// no error is returned. This is postponed to the rule reaction queue
	// to be done after the affected endpoints have been regenerated,
	// otherwise new identities are upserted to the ipcache before we
	// return.
	//
	// Release of these identities will be tied to the corresponding policy
	// in the policy.Repository and released upon policyDelete().
	newlyAllocatedIdentities := make(map[netip.Prefix]*identity.Identity)
	if _, err := d.ipcache.AllocateCIDRs(prefixes, nil, newlyAllocatedIdentities); err != nil {
		_ = d.prefixLengths.Delete(prefixes)
		logger.WithError(err).WithField("prefixes", prefixes).Warn(
			"Failed to allocate identities for CIDRs during policy add")
		resChan <- &PolicyAddResult{
			newRev: 0,
			err:    err,
		}
		return
	}

	// No errors past this point!

	d.policy.Mutex.Lock()

	// removedPrefixes tracks prefixes that we replace in the rules. It is used
	// after we release the policy repository lock.
	var removedPrefixes []netip.Prefix

	// policySelectionWG is used to signal when the updating of all of the
	// caches of endpoints in the rules which were added / updated have been
	// updated.
	var policySelectionWG sync.WaitGroup

	// Get all endpoints at the time rules were added / updated so we can figure
	// out which endpoints to regenerate / bump policy revision.
	allEndpoints := d.endpointManager.GetPolicyEndpoints()

	// Start with all endpoints to be in set for which we need to bump their
	// revision.
	endpointsToBumpRevision := policy.NewEndpointSet(allEndpoints)

	endpointsToRegen := policy.NewEndpointSet(nil)

	if opts != nil {
		if opts.Replace {
			for _, r := range sourceRules {
				oldRules := d.policy.SearchRLocked(r.Labels)
				removedPrefixes = append(removedPrefixes, policy.GetCIDRPrefixes(oldRules)...)
				if len(oldRules) > 0 {
					deletedRules, _, _ := d.policy.DeleteByLabelsLocked(r.Labels)
					deletedRules.UpdateRulesEndpointsCaches(endpointsToBumpRevision, endpointsToRegen, &policySelectionWG)
				}
			}
		}
		if len(opts.ReplaceWithLabels) > 0 {
			oldRules := d.policy.SearchRLocked(opts.ReplaceWithLabels)
			removedPrefixes = append(removedPrefixes, policy.GetCIDRPrefixes(oldRules)...)
			if len(oldRules) > 0 {
				deletedRules, _, _ := d.policy.DeleteByLabelsLocked(opts.ReplaceWithLabels)
				deletedRules.UpdateRulesEndpointsCaches(endpointsToBumpRevision, endpointsToRegen, &policySelectionWG)
			}
		}
	}

	addedRules, newRev := d.policy.AddListLocked(sourceRules)

	// The information needed by the caller is available at this point, signal
	// accordingly.
	resChan <- &PolicyAddResult{
		newRev: newRev,
		err:    nil,
	}

	addedRules.UpdateRulesEndpointsCaches(endpointsToBumpRevision, endpointsToRegen, &policySelectionWG)

	d.policy.Mutex.Unlock()

	if newPrefixLengths && !bpfIPCache.BackedByLPM() {
		// bpf_host needs to be recompiled whenever CIDR policy changed.
		if hostEp := d.endpointManager.GetHostEndpoint(); hostEp != nil {
			logger.Debug("CIDR policy has changed; regenerating host endpoint")
			endpointsToRegen.Insert(hostEp)
			endpointsToBumpRevision.Delete(hostEp)
		}
	}

	// Begin tracking the time taken to deploy newRev to the datapath. The start
	// time is from before the locking above, and thus includes all waits and
	// processing in this function.
	source := ""
	if opts != nil {
		source = opts.Source
	}
	d.endpointManager.CallbackForEndpointsAtPolicyRev(d.ctx, newRev, func(now time.Time) {
		duration, _ := safetime.TimeSinceSafe(policyAddStartTime, logger)
		metrics.PolicyImplementationDelay.WithLabelValues(source).Observe(duration.Seconds())
	})

	// remove prefixes of replaced rules above. Refcounts have been incremented
	// above, so any decrements here will be no-ops for CIDRs that are re-added,
	// and will trigger deletions for those that are no longer used.
	if len(removedPrefixes) > 0 {
		logger.WithField("prefixes", removedPrefixes).Debug("Decrementing replaced CIDR refcounts when adding rules")
		d.ipcache.ReleaseCIDRIdentitiesByCIDR(removedPrefixes)
		d.prefixLengths.Delete(removedPrefixes)
	}

	logger.WithField(logfields.PolicyRevision, newRev).Info("Policy imported via API, recalculating...")

	labels := make([]string, 0, len(sourceRules))
	for _, r := range sourceRules {
		labels = append(labels, r.Labels.GetModel()...)
	}
	err = d.SendNotification(monitorAPI.PolicyUpdateMessage(len(sourceRules), labels, newRev))
	if err != nil {
		logger.WithError(err).WithField(logfields.PolicyRevision, newRev).Warn("Failed to send policy update as monitor notification")
	}

	// Only regenerate endpoints which are needed to be regenerated as a
	// result of the rule update. The rules which were imported most likely
	// do not select all endpoints in the policy repository (and may not
	// select any at all). The "reacting" to rule updates enqueues events
	// for all endpoints. Once all endpoints have events queued up, this
	// function will return.
	//
	// Upserting CIDRs to ipcache is performed after endpoint regeneration
	// and serialized with the corresponding ipcache deletes via the
	// policy reaction queue.
	r := &PolicyReactionEvent{
		d:                 d,
		wg:                &policySelectionWG,
		epsToBumpRevision: endpointsToBumpRevision,
		endpointsToRegen:  endpointsToRegen,
		newRev:            newRev,
		upsertIdentities:  newlyAllocatedIdentities,
	}

	ev := eventqueue.NewEvent(r)
	// This event may block if the RuleReactionQueue is full. We don't care
	// about when it finishes, just that the work it does is done in a serial
	// order.
	_, err = d.policy.RuleReactionQueue.Enqueue(ev)
	if err != nil {
		log.WithError(err).WithField(logfields.PolicyRevision, newRev).Error("enqueue of RuleReactionEvent failed")
	}

	return
}

// PolicyReactionEvent is an event which needs to be serialized after changes
// to a policy repository for a daemon. This currently consists of endpoint
// regenerations / policy revision incrementing for a given endpoint.
type PolicyReactionEvent struct {
	d                 *Daemon
	wg                *sync.WaitGroup
	epsToBumpRevision *policy.EndpointSet
	endpointsToRegen  *policy.EndpointSet
	newRev            uint64
	upsertIdentities  map[netip.Prefix]*identity.Identity // deferred CIDR identity upserts, if any
	releasePrefixes   []netip.Prefix                      // deferred CIDR identity deletes, if any
}

// Handle implements pkg/eventqueue/EventHandler interface.
func (r *PolicyReactionEvent) Handle(res chan interface{}) {
	// Wait until we have calculated which endpoints need to be selected
	// across multiple goroutines.
	r.wg.Wait()
	r.d.reactToRuleUpdates(r.epsToBumpRevision, r.endpointsToRegen, r.newRev, r.upsertIdentities, r.releasePrefixes)
}

// reactToRuleUpdates does the following:
//   - regenerate all endpoints in epsToRegen
//   - bump the policy revision of all endpoints not in epsToRegen, but which are
//     in allEps, to revision rev.
//   - wait for the regenerations to be finished
//   - upsert or delete CIDR identities to the ipcache, as needed.
func (d *Daemon) reactToRuleUpdates(epsToBumpRevision, epsToRegen *policy.EndpointSet, rev uint64, upsertIdentities map[netip.Prefix]*identity.Identity, releasePrefixes []netip.Prefix) {
	var enqueueWaitGroup sync.WaitGroup

	// Release CIDR identities before regenerations have been started, if any. This makes sure
	// the stale identities are not used in policy map classifications after we regenerate the
	// endpoints below.
	if len(releasePrefixes) != 0 {
		d.ipcache.ReleaseCIDRIdentitiesByCIDR(releasePrefixes)
	}

	// Bump revision of endpoints which don't need to be regenerated.
	epsToBumpRevision.ForEachGo(&enqueueWaitGroup, func(epp policy.Endpoint) {
		if epp == nil {
			return
		}
		epp.PolicyRevisionBumpEvent(rev)
	})

	// Regenerate all other endpoints.
	regenMetadata := &regeneration.ExternalRegenerationMetadata{
		Reason:            "policy rules added",
		RegenerationLevel: regeneration.RegenerateWithoutDatapath,
	}
	epsToRegen.ForEachGo(&enqueueWaitGroup, func(ep policy.Endpoint) {
		if ep != nil {
			switch e := ep.(type) {
			case *endpoint.Endpoint:
				// Do not wait for the returned channel as we want this to be
				// ASync
				e.RegenerateIfAlive(regenMetadata)
			default:
				log.Errorf("BUG: endpoint not type of *endpoint.Endpoint, received '%s' instead", e)
			}
		}
	})

	enqueueWaitGroup.Wait()

	// Upsert new identities after regeneration has completed, if any. This makes sure the
	// policy maps are ready to classify packets using the newly allocated identities before
	// they are upserted to the ipcache here.
	if upsertIdentities != nil {
		d.ipcache.UpsertGeneratedIdentities(upsertIdentities, nil)
	}
}

// PolicyDeleteEvent is a wrapper around deletion of policy rules with a given
// set of labels from the policy repository in the daemon.
type PolicyDeleteEvent struct {
	labels labels.LabelArray
	d      *Daemon
}

// Handle implements pkg/eventqueue/EventHandler interface.
func (p *PolicyDeleteEvent) Handle(res chan interface{}) {
	p.d.policyDelete(p.labels, res)
}

// PolicyDeleteResult is a wrapper around the values returned by policyDelete.
// It contains the new revision of a policy repository after deleting a list of
// rules to it, and any error associated with adding rules to said repository.
type PolicyDeleteResult struct {
	newRev uint64
	err    error
}

// PolicyDelete deletes the policy rules with the provided set of labels from
// the policy repository of the daemon.
// Returns the revision number and an error in case it was not possible to
// delete the policy.
func (d *Daemon) PolicyDelete(labels labels.LabelArray) (newRev uint64, err error) {

	p := &PolicyDeleteEvent{
		labels: labels,
		d:      d,
	}
	policyDeleteEvent := eventqueue.NewEvent(p)
	resChan, err := d.policy.RepositoryChangeQueue.Enqueue(policyDeleteEvent)
	if err != nil {
		return 0, fmt.Errorf("enqueue of PolicyDeleteEvent failed: %s", err)
	}

	res, ok := <-resChan
	if ok {
		ress := res.(*PolicyDeleteResult)
		return ress.newRev, ress.err
	}
	return 0, fmt.Errorf("policy deletion event cancelled")
}

func (d *Daemon) policyDelete(labels labels.LabelArray, res chan interface{}) {
	log.WithField(logfields.IdentityLabels, logfields.Repr(labels)).Debug("Policy Delete Request")

	d.policy.Mutex.Lock()

	// policySelectionWG is used to signal when the updating of all of the
	// caches of allEndpoints in the rules which were added / updated have been
	// updated.
	var policySelectionWG sync.WaitGroup

	// Get all endpoints at the time rules were added / updated so we can figure
	// out which endpoints to regenerate / bump policy revision.
	allEndpoints := d.endpointManager.GetPolicyEndpoints()
	// Initially keep all endpoints in set of endpoints which need to have
	// revision bumped.
	epsToBumpRevision := policy.NewEndpointSet(allEndpoints)

	endpointsToRegen := policy.NewEndpointSet(nil)

	deletedRules, rev, deleted := d.policy.DeleteByLabelsLocked(labels)

	// Return an error if a label filter was provided and there are no
	// rules matching it. A deletion request for all policy entries should
	// not fail if no policies are loaded.
	if len(deletedRules) == 0 && len(labels) != 0 {
		rev := d.policy.GetRevision()
		d.policy.Mutex.Unlock()

		err := api.New(DeletePolicyNotFoundCode, "policy not found")

		res <- &PolicyDeleteResult{
			newRev: rev,
			err:    err,
		}
		return
	}
	deletedRules.UpdateRulesEndpointsCaches(epsToBumpRevision, endpointsToRegen, &policySelectionWG)

	res <- &PolicyDeleteResult{
		newRev: rev,
		err:    nil,
	}

	d.policy.Mutex.Unlock()

	// Now that the policies are deleted, we can also attempt to remove
	// all CIDR identities referenced by the deleted rules.
	//
	// We don't treat failures to clean up identities as API failures,
	// because the policy can still successfully be updated. We're just
	// not appropriately performing garbage collection.
	prefixes := policy.GetCIDRPrefixes(deletedRules.AsPolicyRules())
	log.WithField("prefixes", prefixes).Debug("Policy deleted via API, found prefixes...")

	prefixesChanged := d.prefixLengths.Delete(prefixes)
	if !bpfIPCache.BackedByLPM() && prefixesChanged {
		// Only recompile if configuration has changed.
		log.Debug("CIDR policy has changed; recompiling base programs")
		if err := d.Datapath().Loader().Reinitialize(d.ctx, d, d.mtuConfig.GetDeviceMTU(), d.Datapath(), d.l7Proxy); err != nil {
			log.WithError(err).Error("Unable to recompile base programs")
		}

		// bpf_host needs to be recompiled whenever CIDR policy changed.
		if hostEp := d.endpointManager.GetHostEndpoint(); hostEp != nil {
			log.Debug("CIDR policy has changed; regenerating host endpoint")
			endpointsToRegen.Insert(hostEp)
			epsToBumpRevision.Delete(hostEp)
		}
	}

	// Releasing prefixes from ipcache is serialized with the corresponding
	// ipcache upserts via the policy reaction queue. Execution order
	// w.r.t. to endpoint regenerations remains the same, endpoints are
	// regenerated after any prefixes have been removed from the ipcache.
	r := &PolicyReactionEvent{
		d:                 d,
		wg:                &policySelectionWG,
		epsToBumpRevision: epsToBumpRevision,
		endpointsToRegen:  endpointsToRegen,
		newRev:            rev,
		releasePrefixes:   prefixes,
	}

	ev := eventqueue.NewEvent(r)
	// This event may block if the RuleReactionQueue is full. We don't care
	// about when it finishes, just that the work it does is done in a serial
	// order.
	if _, err := d.policy.RuleReactionQueue.Enqueue(ev); err != nil {
		log.WithError(err).WithField(logfields.PolicyRevision, rev).Error("enqueue of RuleReactionEvent failed")
	}
	if err := d.SendNotification(monitorAPI.PolicyDeleteMessage(deleted, labels.GetModel(), rev)); err != nil {
		log.WithError(err).WithField(logfields.PolicyRevision, rev).Warn("Failed to send policy update as monitor notification")
	}

	return
}

type deletePolicy struct {
	daemon *Daemon
}

func newDeletePolicyHandler(d *Daemon) DeletePolicyHandler {
	return &deletePolicy{daemon: d}
}

func (h *deletePolicy) Handle(params DeletePolicyParams) middleware.Responder {
	d := h.daemon
	lbls := labels.ParseSelectLabelArrayFromArray(params.Labels)
	rev, err := d.PolicyDelete(lbls)
	if err != nil {
		return api.Error(DeletePolicyFailureCode, err)
	}

	ruleList := d.policy.SearchRLocked(labels.LabelArray{})
	policy := &models.Policy{
		Revision: int64(rev),
		Policy:   policy.JSONMarshalRules(ruleList),
	}
	return NewDeletePolicyOK().WithPayload(policy)
}

type putPolicy struct {
	daemon *Daemon
}

func newPutPolicyHandler(d *Daemon) PutPolicyHandler {
	return &putPolicy{daemon: d}
}

func (h *putPolicy) Handle(params PutPolicyParams) middleware.Responder {
	d := h.daemon

	var rules policyAPI.Rules
	if err := json.Unmarshal([]byte(params.Policy), &rules); err != nil {
		metrics.PolicyImportErrorsTotal.Inc() // Deprecated in Cilium 1.14, to be removed in 1.15.
		metrics.PolicyChangeTotal.WithLabelValues(metrics.LabelValueOutcomeFail).Inc()
		return NewPutPolicyInvalidPolicy()
	}

	for _, r := range rules {
		if err := r.Sanitize(); err != nil {
			metrics.PolicyImportErrorsTotal.Inc() // Deprecated in Cilium 1.14, to be removed in 1.15.
			metrics.PolicyChangeTotal.WithLabelValues(metrics.LabelValueOutcomeFail).Inc()
			return api.Error(PutPolicyFailureCode, err)
		}
	}

	rev, err := d.PolicyAdd(rules, &policy.AddOptions{Source: metrics.LabelEventSourceAPI})
	if err != nil {
		metrics.PolicyImportErrorsTotal.Inc() // Deprecated in Cilium 1.14, to be removed in 1.15.
		metrics.PolicyChangeTotal.WithLabelValues(metrics.LabelValueOutcomeFail).Inc()
		return api.Error(PutPolicyFailureCode, err)
	}
	metrics.PolicyChangeTotal.WithLabelValues(metrics.LabelValueOutcomeSuccess).Inc()

	policy := &models.Policy{
		Revision: int64(rev),
		Policy:   policy.JSONMarshalRules(rules),
	}
	return NewPutPolicyOK().WithPayload(policy)
}

type getPolicy struct {
	repo *policy.Repository
}

func newGetPolicyHandler(r *policy.Repository) GetPolicyHandler {
	return &getPolicy{repo: r}
}

func (h *getPolicy) Handle(params GetPolicyParams) middleware.Responder {
	repository := h.repo
	repository.Mutex.RLock()
	defer repository.Mutex.RUnlock()

	lbls := labels.ParseSelectLabelArrayFromArray(params.Labels)
	ruleList := repository.SearchRLocked(lbls)

	// Error if labels have been specified but no entries found, otherwise,
	// return empty list
	if len(ruleList) == 0 && len(lbls) != 0 {
		return NewGetPolicyNotFound()
	}

	policy := &models.Policy{
		Revision: int64(repository.GetRevision()),
		Policy:   policy.JSONMarshalRules(ruleList),
	}
	return NewGetPolicyOK().WithPayload(policy)
}

type getPolicySelectors struct {
	daemon *Daemon
}

func newGetPolicyCacheHandler(d *Daemon) GetPolicySelectorsHandler {
	return &getPolicySelectors{daemon: d}
}

func (h *getPolicySelectors) Handle(params GetPolicySelectorsParams) middleware.Responder {
	return NewGetPolicySelectorsOK().WithPayload(h.daemon.policy.GetSelectorCache().GetModel())
}
