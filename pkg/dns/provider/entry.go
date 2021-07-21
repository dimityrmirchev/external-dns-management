/*
 * Copyright 2019 SAP SE or an SAP affiliate company. All rights reserved. This file is licensed under the Apache Software License, v. 2 except as noted otherwise in the LICENSE file
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *      http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 *
 */

package provider

import (
	"fmt"
	"net"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gardener/controller-manager-library/pkg/resources/access"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/gardener/external-dns-management/pkg/dns"
	"github.com/gardener/external-dns-management/pkg/dns/provider/statistic"

	"github.com/gardener/controller-manager-library/pkg/controllermanager/controller/reconcile"
	"github.com/gardener/controller-manager-library/pkg/logger"
	"github.com/gardener/controller-manager-library/pkg/resources"
	"github.com/gardener/controller-manager-library/pkg/utils"

	api "github.com/gardener/external-dns-management/pkg/apis/dns/v1alpha1"
	dnsutils "github.com/gardener/external-dns-management/pkg/dns/utils"

	corev1 "k8s.io/api/core/v1"
)

const MSG_PRESERVED = "errorneous entry preserved in provider"

type EntryPremise struct {
	ptypes   utils.StringSet
	ptype    string
	provider DNSProvider
	fallback DNSProvider // provider with correct zone, but outside selection (only set if provider == nil)
	zoneid   string

	// non-identifying fields
	zonedomain string
}

func (this *EntryPremise) Match(p *EntryPremise) bool {
	return this.ptype == p.ptype && this.provider == p.provider && this.zoneid == p.zoneid && this.fallback == p.fallback
}

func (this *EntryPremise) NotifyChange(p *EntryPremise) string {
	r := []string{}
	if this.ptype != p.ptype {
		r = append(r, fmt.Sprintf("provider type (%s -> %s)", this.ptype, p.ptype))
	}
	if this.provider != p.provider {
		r = append(r, fmt.Sprintf("provider (%s -> %s)", Provider(this.provider), Provider(p.provider)))
	}
	if this.zoneid != p.zoneid {
		r = append(r, fmt.Sprintf("zone (%s -> %s)", this.zoneid, p.zoneid))
	}
	if this.fallback != p.fallback {
		r = append(r, fmt.Sprintf("fallback (%s -> %s)", Provider(this.fallback), Provider(p.fallback)))
	}
	if len(r) == 0 {
		return ""
	}
	return "premise changed: " + strings.Join(r, ", ")
}

type EntryVersion struct {
	object       *dnsutils.DNSEntryObject
	providername resources.ObjectName
	dnsname      string
	targets      Targets
	mappings     map[string][]string
	warnings     []string

	status api.DNSEntryStatus

	interval    int64
	responsible bool
	valid       bool
	duplicate   bool
	obsolete    bool
}

func NewEntryVersion(object *dnsutils.DNSEntryObject, old *Entry) *EntryVersion {
	v := &EntryVersion{
		object:   object,
		dnsname:  object.DNSEntry().Spec.DNSName,
		targets:  Targets{},
		mappings: map[string][]string{},
	}
	if old != nil {
		v.status = old.status
	} else {
		v.status = *object.Status()
	}
	return v
}

func (this *EntryVersion) RequiresUpdateFor(e *EntryVersion) (reasons []string) {
	if this.dnsname != e.dnsname {
		reasons = append(reasons, "dnsname changed")
	}
	if !utils.Int64Equal(this.status.TTL, e.status.TTL) {
		reasons = append(reasons, "ttl changed")
	}
	if this.valid != e.valid {
		reasons = append(reasons, "validation state changed")
	}
	if this.ZoneId() != e.ZoneId() {
		reasons = append(reasons, "zone changed")
	}
	if this.OwnerId() != e.OwnerId() {
		reasons = append(reasons, "ownerid changed")
	}
	if this.targets.DifferFrom(e.targets) {
		reasons = append(reasons, "targets changed")
	}
	if this.State() != e.State() {
		if e.State() != api.STATE_READY {
			reasons = append(reasons, "state changed")
		}
	}
	if this.obsolete != e.obsolete {
		reasons = append(reasons, "provider responsibility changed")
	}
	return
}

func (this *EntryVersion) IsValid() bool {
	return this.valid
}

func (this *EntryVersion) KeepRecords() bool {
	return this.IsValid() || this.status.State != api.STATE_INVALID
}

func (this *EntryVersion) IsDeleting() bool {
	return this.object.IsDeleting()
}

func (this *EntryVersion) Object() *dnsutils.DNSEntryObject {
	return this.object
}

func (this *EntryVersion) Message() string {
	return utils.StringValue(this.status.Message)
}

func (this *EntryVersion) ZoneId() string {
	return utils.StringValue(this.status.Zone)
}

func (this *EntryVersion) State() string {
	return this.status.State
}

func (this *EntryVersion) ClusterKey() resources.ClusterObjectKey {
	return this.object.ClusterKey()
}

func (this *EntryVersion) ObjectName() resources.ObjectName {
	return this.object.ObjectName()
}

func (this *EntryVersion) DNSName() string {
	return this.dnsname
}

func (this *EntryVersion) Targets() Targets {
	return this.targets
}

func (this *EntryVersion) Description() string {
	return this.object.Description()
}

func (this *EntryVersion) TTL() int64 {
	return utils.Int64Value(this.status.TTL, 0)
}

func (this *EntryVersion) Interval() int64 {
	return this.interval
}

func (this *EntryVersion) IsResponsible() bool {
	return this.responsible
}

func (this *EntryVersion) ProviderType() string {
	return utils.StringValue(this.status.ProviderType)
}

func (this *EntryVersion) ProviderName() resources.ObjectName {
	return this.providername
}

func (this *EntryVersion) OwnerId() string {
	if this.object.GetOwnerId() != nil {
		return *this.object.GetOwnerId()
	}
	return ""
}

func complete(logger logger.LogContext, state *state, spec *api.DNSEntrySpec, object resources.Object, prefix string) error {
	if spec.Reference != nil && spec.Reference.Name != "" {
		ns := spec.Reference.Namespace
		if ns == "" {
			ns = object.GetNamespace()
		}
		dnsref := resources.NewObjectName(ns, spec.Reference.Name)
		logger.Infof("completeing spec by reference: %s%s", prefix, dnsref)

		cur := object.ClusterKey()
		key := resources.NewClusterKey(cur.Cluster(), cur.GroupKind(), dnsref.Namespace(), dnsref.Name())
		state.references.AddRef(cur, key)

		ref, err := object.GetResource().GetCached(dnsref)
		if err != nil {
			if errors.IsNotFound(err) {
				err = fmt.Errorf("entry reference %s%q not found", prefix, dnsref)
			}
			logger.Warn(err)
			return err
		}
		err = access.CheckAccessWithRealms(object, "use", ref, state.realms)
		if err != nil {
			return fmt.Errorf("%s%s", prefix, err)
		}
		rspec := *dnsutils.DNSEntry(ref).Spec()
		err = complete(logger, state, &rspec, ref, fmt.Sprintf("%s%s->", prefix, dnsref))
		if err != nil {
			return err
		}

		if spec.Targets != nil {
			return fmt.Errorf("%stargets specified together with entry reference", prefix)
		}
		if spec.Text != nil {
			err = fmt.Errorf("%stext specified together with entry reference", prefix)
			return err
		}
		spec.Targets = rspec.Targets
		spec.Text = rspec.Text

		if spec.TTL == nil {
			spec.TTL = rspec.TTL
		}
		if spec.OwnerId == nil {
			spec.OwnerId = rspec.OwnerId
		}
		if spec.CNameLookupInterval == nil {
			spec.CNameLookupInterval = rspec.CNameLookupInterval
		}
	} else {
		state.references.DelRef(object.ClusterKey())
	}
	return nil
}

func validate(logger logger.LogContext, state *state, entry *EntryVersion, p *EntryPremise) (effspec api.DNSEntrySpec, targets Targets, warnings []string, err error) {
	effspec = entry.object.DNSEntry().Spec

	targets = Targets{}
	warnings = []string{}

	check := entry.object.GetDNSName()
	if strings.HasPrefix(check, "*.") {
		check = check[2:]
	} else if strings.HasPrefix(check, "_") {
		check = check[1:]
	}

	if errs := validation.IsDNS1123Subdomain(check); errs != nil {
		if werrs := validation.IsWildcardDNS1123Subdomain(check); werrs != nil {
			err = fmt.Errorf("%q is no valid dns name (%v)", check, append(errs, werrs...))
			return
		}
	}

	err = complete(logger, state, &effspec, entry.object, "")
	if err != nil {
		return
	}

	if p.zonedomain == entry.dnsname {
		err = fmt.Errorf("usage of dns name (%s) identical to domain of hosted zone (%s) is not supported",
			p.zonedomain, p.zoneid)
		return
	}
	if len(effspec.Targets) > 0 && len(effspec.Text) > 0 {
		err = fmt.Errorf("only Text or Targets possible: %s", err)
		return
	}
	if effspec.TTL != nil && (*effspec.TTL == 0 || *effspec.TTL < 0) {
		err = fmt.Errorf("TTL must be greater than zero: %s", err)
		return
	}

	for i, t := range effspec.Targets {
		if strings.TrimSpace(t) == "" {
			err = fmt.Errorf("target %d must not be empty", i+1)
			return
		}
		var new Target
		new, err = NewTargetFromEntryVersion(t, entry)
		// we should ignore unsupported addresses since the user might not influence
		// the provider assigned ip address type
		if err != nil {
			continue
		}
		if targets.Has(new) {
			warnings = append(warnings, fmt.Sprintf("dns entry %q has duplicate target %q", entry.ObjectName(), new))
		} else {
			targets = append(targets, new)
		}
	}
	tcnt := 0
	for _, t := range effspec.Text {
		if t == "" {
			warnings = append(warnings, fmt.Sprintf("dns entry %q has empty text", entry.ObjectName()))
			continue
		}
		new := NewText(t, entry)
		if targets.Has(new) {
			warnings = append(warnings, fmt.Sprintf("dns entry %q has duplicate text %q", entry.ObjectName(), new))
		} else {
			targets = append(targets, new)
			tcnt++
		}
	}
	if len(effspec.Text) > 0 && tcnt == 0 {
		err = fmt.Errorf("dns entry has only empty text")
		return
	}

	if len(targets) == 0 {
		err = fmt.Errorf("no target or text specified")
	}
	return
}

func validateOwner(logger logger.LogContext, state *state, entry *EntryVersion) error {
	effspec := entry.object.DNSEntry().Spec

	if utils.StringValue(effspec.OwnerId) != "" {
		if !state.ownerCache.IsResponsibleFor(*effspec.OwnerId) && !state.ownerCache.IsResponsiblePendingFor(*effspec.OwnerId) {
			return fmt.Errorf("unknown owner id '%s'", *effspec.OwnerId)
		}
	}
	return nil
}

func (this *EntryVersion) Setup(logger logger.LogContext, state *state, p *EntryPremise, op string, err error, config Config, old *Entry) reconcile.Status {
	hello := dnsutils.NewLogMessage("%s ENTRY: %s, zoneid: %s, handler: %s, provider: %s, ref %+v", op, this.Object().Status().State, p.zoneid, p.ptype, Provider(p.provider), this.Object().Spec().Reference)

	this.valid = false
	this.responsible = false
	spec := this.object.DNSEntry().Spec

	///////////// handle type responsibility

	if !utils.IsEmptyString(this.object.Status().ProviderType) && p.ptype == "" {
		// other controller claimed responsibility?
		this.status.ProviderType = this.object.Status().ProviderType
	}

	if utils.IsEmptyString(this.status.ProviderType) || (p.zoneid != "" && *this.status.ProviderType != p.ptype) {
		if p.zoneid == "" {
			// mark unassigned foreign entries as erroneous
			if this.object.GetCreationTimestamp().Add(config.RescheduleDelay).After(time.Now()) {
				state.RemoveFinalizer(this.object)
				return reconcile.Succeeded(logger).RescheduleAfter(config.RescheduleDelay)
			}
			hello.Infof(logger)
			logger.Infof("probably no responsible controller found (%s) -> mark as error", err)
			this.status.Provider = nil
			this.status.ProviderType = nil
			this.status.Zone = nil
			msg := "No responsible provider found"
			if err != nil {
				msg = fmt.Sprintf("%s: %s", msg, err)
			}
			err := this.updateStatus(logger, api.STATE_ERROR, msg)
			if err != nil {
				return reconcile.Delay(logger, err)
			}
		} else {
			// assign entry to actual type
			hello.Infof(logger)
			logger.Infof("assigning to provider type %q responsible for zone %s", p.ptype, p.zoneid)
			this.status.State = api.STATE_PENDING
			this.status.Message = StatusMessage("waiting for dns reconciliation")
		}
	}

	if p.zoneid == "" && !utils.IsEmptyString(this.status.ProviderType) && p.ptypes.Contains(*this.status.ProviderType) {
		// revoke assignment to actual type
		oldType := utils.StringValue(this.status.ProviderType)
		hello.Infof(logger, "revoke assignment to %s", oldType)
		this.status.Provider = nil
		this.status.ProviderType = nil
		this.status.Zone = nil
		err := this.updateStatus(logger, "", "not valid for known provider anymore -> releasing provider type %s", oldType)
		if err != nil {
			return reconcile.Delay(logger, err)
		}
	}

	if p.zoneid == "" || p.ptype == "" {
		return reconcile.RepeatOnError(logger, state.RemoveFinalizer(this.object))
	}

	provider := ""
	this.status.Zone = &p.zoneid
	this.status.ProviderType = &p.ptype
	this.responsible = true
	if p.provider != nil {
		this.providername = p.provider.ObjectName()
		provider = p.provider.ObjectName().String()
		this.status.Provider = &provider
		defaultTTL := p.provider.DefaultTTL()
		this.status.TTL = &defaultTTL
		if spec.TTL != nil {
			this.status.TTL = spec.TTL
		}
	} else {
		this.providername = nil
		this.status.Provider = nil
		this.status.TTL = nil
	}

	///////////// validate

	if verr := validateOwner(logger, state, this); verr != nil {
		hello.Infof(logger, "owner validation failed: %s", verr)

		this.UpdateStatus(logger, api.STATE_STALE, verr.Error())
		return reconcile.Failed(logger, verr)
	}

	spec, targets, warnings, verr := validate(logger, state, this, p)
	if p.provider != nil && spec.TTL != nil {
		this.status.TTL = spec.TTL
	}

	if verr != nil {
		hello.Infof(logger, "validation failed: %s", verr)

		this.UpdateStatus(logger, api.STATE_INVALID, verr.Error())
		return reconcile.Failed(logger, verr)
	}

	///////////// handle

	hello.Infof(logger, "validation ok")

	if this.IsDeleting() {
		logger.Infof("update state to %s", api.STATE_DELETING)
		this.status.State = api.STATE_DELETING
		this.status.Message = StatusMessage("entry is scheduled to be deleted")
		this.valid = true
	} else {
		this.warnings = warnings
		targets, multiCName, multiOk := normalizeTargets(logger, this.object, targets...)
		if multiCName {
			this.interval = int64(600)
			if spec.CNameLookupInterval != nil && *spec.CNameLookupInterval > 0 {
				this.interval = *spec.CNameLookupInterval
			}
			if len(targets) == 0 {
				msg := "targets cannot be resolved to any valid IPv4 address"
				if !multiOk {
					msg = "too many targets"
					this.interval = int64(84600)
				}

				verr := fmt.Errorf(msg)
				hello.Infof(logger, msg)

				state := api.STATE_INVALID
				// if DNS lookup fails temporarily, go to state STALE
				if this.status.State == api.STATE_READY || this.status.State == api.STATE_STALE {
					state = api.STATE_STALE
				}
				this.UpdateStatus(logger, state, verr.Error())
				return reconcile.Recheck(logger, verr, time.Duration(this.interval)*time.Second)
			}
		} else {
			this.interval = 0
		}

		this.targets = targets
		if err != nil {
			if this.status.State != api.STATE_STALE {
				if this.status.State == api.STATE_READY && (p.provider != nil && !p.provider.IsValid()) {
					this.status.State = api.STATE_STALE
				} else {
					this.status.State = api.STATE_ERROR
				}
				this.status.Message = StatusMessage(err.Error())
			} else {
				if strings.HasPrefix(*this.status.Message, MSG_PRESERVED) {
					this.status.Message = StatusMessage(MSG_PRESERVED + ": " + err.Error())
				} else {
					this.status.Message = StatusMessage(err.Error())
				}
			}
		} else {
			if p.zoneid == "" {
				this.status.State = api.STATE_ERROR
				this.status.Provider = nil
				this.status.Message = StatusMessagef("no provider found for %q", this.dnsname)
			} else {
				if p.provider.IsValid() {
					this.valid = true
				} else {
					this.status.State = api.STATE_STALE
					this.status.Message = StatusMessagef("provider %q not valid", p.provider.ObjectName())
				}
			}
		}
	}

	logger.Infof("%s: valid: %t, message: %s%s", this.status.State, this.valid, utils.StringValue(this.status.Message), errorValue(", err: %s", err))
	logmsg := dnsutils.NewLogMessage("update entry status")
	f := func(data resources.ObjectData) (bool, error) {
		e := data.(*api.DNSEntry)
		mod := &utils.ModificationState{}
		if p.zoneid != "" {
			mod.AssureStringPtrValue(&e.Status.ProviderType, p.ptype)
		}
		mod.AssureStringValue(&e.Status.State, this.status.State).
			AssureStringPtrPtr(&e.Status.Message, this.status.Message).
			AssureStringPtrPtr(&e.Status.Zone, this.status.Zone).
			AssureStringPtrPtr(&e.Status.Provider, this.status.Provider)
		if mod.IsModified() {
			dnsutils.SetLastUpdateTime(&e.Status.LastUptimeTime)
			logmsg.Infof(logger)
		}
		return mod.IsModified(), nil
	}
	_, err = this.object.ModifyStatus(f)
	return reconcile.DelayOnError(logger, err)
}

func (this *EntryVersion) updateStatus(logger logger.LogContext, state, msg string, args ...interface{}) error {
	logmsg := dnsutils.NewLogMessage(msg, args...)
	f := func(data resources.ObjectData) (bool, error) {
		e := data.(*api.DNSEntry)
		mod := (&utils.ModificationState{}).
			AssureStringPtrPtr(&e.Status.ProviderType, this.status.ProviderType).
			AssureStringValue(&e.Status.State, state).
			AssureStringPtrValue(&e.Status.Message, logmsg.Get()).
			AssureStringPtrPtr(&e.Status.Zone, this.status.Zone).
			AssureStringPtrPtr(&e.Status.Provider, this.status.Provider).
			AssureInt64PtrPtr(&e.Status.TTL, this.status.TTL)
		if state != "" && e.Status.ObservedGeneration < this.object.GetGeneration() {
			mod.AssureInt64Value(&e.Status.ObservedGeneration, this.object.GetGeneration())
		}
		if utils.StringValue(this.status.Provider) == "" {
			if e.Status.Targets != nil {
				e.Status.Targets = nil
				mod.Modify(true)
			}
		}
		if mod.IsModified() {
			logmsg.Infof(logger)
		}
		return mod.IsModified(), nil
	}
	_, err := this.object.ModifyStatus(f)
	this.object.Event(corev1.EventTypeNormal, "reconcile", logmsg.Get())
	return err
}

func (this *EntryVersion) UpdateStatus(logger logger.LogContext, state string, msg string) (bool, error) {
	f := func(data resources.ObjectData) (bool, error) {
		o := data.(*api.DNSEntry)
		if state == api.STATE_PENDING && o.Status.State != "" {
			return false, nil
		}
		mod := &utils.ModificationState{}

		if state == api.STATE_READY {
			mod.AssureInt64PtrPtr(&o.Status.TTL, this.status.TTL)
			list, msg := targetList(this.targets)
			if !reflect.DeepEqual(list, o.Status.Targets) {
				o.Status.Targets = list
				logger.Info(msg)
				mod.Modify(true)
			}
			if this.status.Provider != nil {
				mod.AssureStringPtrPtr(&o.Status.Provider, this.status.Provider)
			}
		} else if state != api.STATE_STALE {
			if o.Status.Targets != nil {
				o.Status.Targets = nil
				mod.Modify(true)
			}
		}
		mod.AssureInt64Value(&o.Status.ObservedGeneration, o.Generation)
		if !(this.status.State == api.STATE_STALE && this.status.State == state) {
			mod.AssureStringPtrValue(&o.Status.Message, msg)
			this.status.Message = &msg
		}
		mod.AssureStringValue(&o.Status.State, state)
		this.status.State = state
		if mod.IsModified() {
			dnsutils.SetLastUpdateTime(&o.Status.LastUptimeTime)
			logger.Infof("update state of '%s/%s' to %s (%s)", o.Namespace, o.Name, state, msg)
		}
		return mod.IsModified(), nil
	}
	return this.object.ModifyStatus(f)
}

func targetList(targets Targets) ([]string, string) {
	list := []string{}
	msg := "update effective targets: ["
	sep := ""
	for _, t := range targets {
		list = append(list, t.GetHostName())
		msg = fmt.Sprintf("%s%s%s", msg, sep, t)
		sep = ", "
	}
	msg = msg + "]"
	return list, msg
}

func normalizeTargets(logger logger.LogContext, object *dnsutils.DNSEntryObject, targets ...Target) (Targets, bool, bool) {
	multiCNAME := len(targets) > 1 && targets[0].GetRecordType() == dns.RS_CNAME
	if !multiCNAME {
		return targets, false, false
	}

	result := make(Targets, 0, len(targets))
	if len(targets) > 11 {
		w := fmt.Sprintf("too many CNAME targets: %d", len(targets))
		logger.Warn(w)
		object.Event(corev1.EventTypeWarning, "dnslookup restriction", w)
		return result, true, false
	}
	for _, t := range targets {
		ipv4addrs, ipv6addrs, err := lookupHosts(t.GetHostName())
		if err == nil {
		outerV4:
			for _, addr := range ipv4addrs {
				for _, old := range result {
					if old.GetHostName() == addr {
						continue outerV4
					}
				}
				result = append(result, NewTarget(dns.RS_A, addr, t.GetEntry()))
			}
		outerV6:
			for _, addr := range ipv6addrs {
				for _, old := range result {
					if old.GetHostName() == addr {
						continue outerV6
					}
				}
				result = append(result, NewTarget(dns.RS_AAAA, addr, t.GetEntry()))
			}
		} else {
			w := fmt.Sprintf("cannot lookup '%s': %s", t.GetHostName(), err)
			logger.Warn(w)
			object.Event(corev1.EventTypeNormal, "dnslookup", w)
		}
	}
	return result, true, true
}

func lookupHosts(hostname string) ([]string, []string, error) {
	ips, err := net.LookupIP(hostname)
	if err != nil {
		return nil, nil, err
	}
	ipv4addrs := make([]string, 0, len(ips))
	ipv6addrs := make([]string, 0, len(ips))
	for _, ip := range ips {
		if ip.To4() != nil {
			ipv4addrs = append(ipv4addrs, ip.String())
		} else if ip.To16() != nil {
			ipv6addrs = append(ipv6addrs, ip.String())
		}
	}
	if len(ipv4addrs) == 0 && len(ipv6addrs) == 0 {
		return nil, nil, fmt.Errorf("%s has no IPv4/IPv6 address (of %d addresses)", hostname, len(ips))
	}
	return ipv4addrs, ipv6addrs, nil
}

///////////////////////////////////////////////////////////////////////////////

type Entry struct {
	lock       sync.Mutex
	key        string
	createdAt  time.Time
	modified   bool
	activezone string
	state      *state

	*EntryVersion
}

func NewEntry(v *EntryVersion, state *state) *Entry {
	return &Entry{
		key:          v.ObjectName().String(),
		EntryVersion: v,
		state:        state,
		modified:     true,
		createdAt:    time.Now(),
		activezone:   utils.StringValue(v.status.Zone),
	}
}

func (this *Entry) RemoveFinalizer() error {
	return this.state.RemoveFinalizer(this.object.DeepCopy())
}

func (this *Entry) Trigger(logger logger.LogContext) {
	this.state.TriggerEntry(logger, this)
}

func (this *Entry) IsActive() bool {
	id := this.OwnerId()
	if id == "" {
		id = this.state.config.Ident
	}
	return this.state.ownerCache.IsResponsibleFor(id)
}

func (this *Entry) IsModified() bool {
	return this.modified
}

func (this *Entry) CreatedAt() time.Time {
	return this.createdAt
}

func (this *Entry) Update(logger logger.LogContext, new *EntryVersion) *Entry {
	if this.DNSName() != new.DNSName() {
		return NewEntry(new, this.state)
	}

	reasons := this.RequiresUpdateFor(new)
	if len(reasons) != 0 {
		logger.Infof("update actual entry: valid: %t  %v", new.IsValid(), reasons)
		if this.targets.DifferFrom(new.targets) && !new.IsDeleting() {
			logger.Infof("targets differ from internal state")
			for _, w := range new.warnings {
				logger.Warn(w)
				this.object.Event(corev1.EventTypeNormal, "reconcile", w)
			}
			for dns, m := range new.mappings {
				msg := fmt.Sprintf("mapping cname %q to %v", dns, m)
				logger.Info(msg)
				this.object.Event(corev1.EventTypeNormal, "dnslookup", msg)
			}
			_, msg := targetList(new.targets)
			logger.Infof("%s", msg)
		}
		this.modified = true
	}
	this.EntryVersion = new

	if new.valid && this.status.State == api.STATE_STALE {
		this.modified = true
	}

	return this
}

func (this *Entry) Before(e *Entry) bool {
	if e == nil {
		return true
	}
	if this.Object().GetCreationTimestamp().Time.Equal(e.Object().GetCreationTimestamp().Time) {
		// for entries created at same time compare objectname to define strict order
		return strings.Compare(this.ObjectName().String(), e.ObjectName().String()) < 0
	}
	return this.Object().GetCreationTimestamp().Time.Before(e.Object().GetCreationTimestamp().Time)
}

func (this *Entry) updateStatistic(statistic *statistic.EntryStatistic) {
	this.lock.Lock()
	defer this.lock.Unlock()
	statistic.Owners.Inc(this.OwnerId(), this.ProviderType(), this.ProviderName())
	statistic.Providers.Inc(this.ProviderType(), this.ProviderName())
}

////////////////////////////////////////////////////////////////////////////////
// Entries
////////////////////////////////////////////////////////////////////////////////

type Entries map[resources.ObjectName]*Entry

func (this Entries) AddResponsibleTo(list *EntryList) {
	for _, e := range this {
		if e.IsResponsible() {
			*list = append(*list, e)
		}
	}
}

func (this Entries) AddActiveZoneTo(zoneid string, list *EntryList) {
	for _, e := range this {
		if e.activezone == zoneid {
			*list = append(*list, e)
		}
	}
}

func (this Entries) AddEntry(entry *Entry) *Entry {
	old := this[entry.ObjectName()]
	this[entry.ObjectName()] = entry
	if old != nil && old != entry {
		return old
	}
	return nil
}

func (this Entries) Delete(e *Entry) {
	if this[e.ObjectName()] == e {
		delete(this, e.ObjectName())
	}
}

////////////////////////////////////////////////////////////////////////////////
// synchronizedEntries
////////////////////////////////////////////////////////////////////////////////

type synchronizedEntries struct {
	lock    sync.Mutex
	entries Entries
}

func newSynchronizedEntries() *synchronizedEntries {
	return &synchronizedEntries{entries: Entries{}}
}

func (this *synchronizedEntries) AddResponsibleTo(list *EntryList) {
	this.lock.Lock()
	defer this.lock.Unlock()
	this.entries.AddResponsibleTo(list)
}

func (this *synchronizedEntries) AddActiveZoneTo(zoneid string, list *EntryList) {
	this.lock.Lock()
	defer this.lock.Unlock()
	this.entries.AddActiveZoneTo(zoneid, list)
}

func (this *synchronizedEntries) AddEntry(entry *Entry) *Entry {
	this.lock.Lock()
	defer this.lock.Unlock()
	return this.entries.AddEntry(entry)
}

func (this *synchronizedEntries) Delete(e *Entry) {
	this.lock.Lock()
	defer this.lock.Unlock()
	this.entries.Delete(e)
}

////////////////////////////////////////////////////////////////////////////////
// EntryList
////////////////////////////////////////////////////////////////////////////////

type EntryList []*Entry

func (this EntryList) Len() int {
	return len(this)
}

func (this EntryList) Less(i, j int) bool {
	return strings.Compare(this[i].key, this[j].key) < 0
}

func (this EntryList) Swap(i, j int) {
	this[i], this[j] = this[j], this[i]
}

func (this EntryList) Sort() {
	sort.Sort(this)
}

func (this EntryList) Lock() {
	this.Sort()
	for _, e := range this {
		e.lock.Lock()
	}
}

func (this EntryList) Unlock() {
	for _, e := range this {
		e.lock.Unlock()
	}
}

func (this EntryList) UpdateStatistic(statistic *statistic.EntryStatistic) {
	for _, e := range this {
		e.updateStatistic(statistic)
	}
}

func StatusMessage(s string) *string {
	return &s
}
func StatusMessagef(msgfmt string, args ...interface{}) *string {
	return StatusMessage(fmt.Sprintf(msgfmt, args...))
}

func Provider(p DNSProvider) string {
	if p == nil {
		return "<none>"
	}
	return p.ObjectName().String()
}
