/*
Copyright 2019 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or collectoried.
See the License for the specific language governing permissions and
limitations under the License.
*/

package services

import (
	"context"
	"sync"
	"time"

	"github.com/gravitational/teleport/api/constants"
	apidefaults "github.com/gravitational/teleport/api/defaults"
	"github.com/gravitational/teleport/api/types"
	"github.com/gravitational/teleport/lib/defaults"
	"github.com/gravitational/teleport/lib/utils"

	"github.com/gravitational/trace"
	"github.com/jonboulle/clockwork"
	"github.com/sirupsen/logrus"
)

// resourceCollector is a generic interface for maintaining an up-to-date view
// of a resource set being monitored. Used in conjunction with resourceWatcher.
type resourceCollector interface {
	// resourceKind specifies the resource kind to watch.
	resourceKind() string
	// getResourcesAndUpdateCurrent is called when the resources should be
	// (re-)fetched directly.
	getResourcesAndUpdateCurrent(context.Context) error
	// processEventAndUpdateCurrent is called when a watcher event is received.
	processEventAndUpdateCurrent(context.Context, types.Event)
	// notifyStale is called when the maximum acceptable staleness (if specified)
	// is exceeded.
	notifyStale()
}

// ResourceWatcherConfig configures resource watcher.
type ResourceWatcherConfig struct {
	// Component is a component used in logs.
	Component string
	// Log is a logger.
	Log logrus.FieldLogger
	// MaxRetryPeriod is the maximum retry period on failed watchers.
	MaxRetryPeriod time.Duration
	// RefetchPeriod is a period after which to explicitly refetch the resources.
	// It is to protect against unexpected cache syncing issues.
	RefetchPeriod time.Duration
	// Clock is used to control time.
	Clock clockwork.Clock
	// Client is used to create new watchers.
	Client types.Events
	// MaxStaleness is a maximum acceptable staleness for the locally maintained
	// resources, zero implies no staleness detection.
	MaxStaleness time.Duration
	// ResetC is a channel to notify of internal watcher reset (used in tests).
	ResetC chan time.Duration
}

// CheckAndSetDefaults checks parameters and sets default values.
func (cfg *ResourceWatcherConfig) CheckAndSetDefaults() error {
	if cfg.Component == "" {
		return trace.BadParameter("missing parameter Component")
	}
	if cfg.Log == nil {
		cfg.Log = logrus.StandardLogger()
	}
	if cfg.MaxRetryPeriod == 0 {
		cfg.MaxRetryPeriod = defaults.MaxWatcherBackoff
	}
	if cfg.RefetchPeriod == 0 {
		cfg.RefetchPeriod = defaults.LowResPollingPeriod
	}
	if cfg.Clock == nil {
		cfg.Clock = clockwork.NewRealClock()
	}
	if cfg.Client == nil {
		return trace.BadParameter("missing parameter Client")
	}
	if cfg.ResetC == nil {
		cfg.ResetC = make(chan time.Duration, 1)
	}
	return nil
}

// newResourceWatcher returns a new instance of resourceWatcher.
// It is the caller's responsibility to verify the inputs' validity
// incl. cfg.CheckAndSetDefaults.
func newResourceWatcher(ctx context.Context, collector resourceCollector, cfg ResourceWatcherConfig) (*resourceWatcher, error) {
	retry, err := utils.NewLinear(utils.LinearConfig{
		First:  utils.HalfJitter(cfg.MaxRetryPeriod / 10),
		Step:   cfg.MaxRetryPeriod / 5,
		Max:    cfg.MaxRetryPeriod,
		Jitter: utils.NewHalfJitter(),
		Clock:  cfg.Clock,
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}
	cfg.Log = cfg.Log.WithField("resource-kind", collector.resourceKind())
	ctx, cancel := context.WithCancel(ctx)
	p := &resourceWatcher{
		ResourceWatcherConfig: cfg,
		collector:             collector,
		ctx:                   ctx,
		cancel:                cancel,
		retry:                 retry,
		LoopC:                 make(chan struct{}),
		StaleC:                make(chan struct{}, 1),
	}
	go p.runWatchLoop()
	return p, nil
}

// resourceWatcher monitors additions, updates and deletions
// to a set of resources.
type resourceWatcher struct {
	ResourceWatcherConfig
	collector resourceCollector

	// ctx is a context controlling the lifetime of this resourceWatcher
	// instance.
	ctx    context.Context
	cancel context.CancelFunc

	// retry is used to manage backoff logic for watchers.
	retry utils.Retry

	// failureStartedAt records when the current sync failures were first
	// detected, zero if there are no failures present.
	failureStartedAt time.Time

	// LoopC is a channel to check whether the watch loop is running
	// (used in tests).
	LoopC chan struct{}

	// StaleC is a channel that can trigger the condition of resource staleness
	// (used in tests).
	StaleC chan struct{}
}

// Done returns a channel that signals resource watcher closure.
func (p *resourceWatcher) Done() <-chan struct{} {
	return p.ctx.Done()
}

// Close closes the resource watcher and cancels all the functions.
func (p *resourceWatcher) Close() {
	p.cancel()
}

// hasStaleView returns true when the local view has failed to be updated
// for longer than the MaxStaleness bound.
func (p *resourceWatcher) hasStaleView() bool {
	// Used for testing stale lock views.
	select {
	case <-p.StaleC:
		return true
	default:
	}

	if p.MaxStaleness == 0 || p.failureStartedAt.IsZero() {
		return false
	}
	return p.Clock.Since(p.failureStartedAt) > p.MaxStaleness
}

// runWatchLoop runs a watch loop.
func (p *resourceWatcher) runWatchLoop() {
	for {
		p.Log.Debug("Starting watch.")
		err := p.watch()

		select {
		case <-p.ctx.Done():
			return
		default:
		}

		if err != nil && p.failureStartedAt.IsZero() {
			// Note that failureStartedAt is zeroed in the watch routine immediately
			// after the local resource set has been successfully updated.
			p.failureStartedAt = p.Clock.Now()
		}
		if p.hasStaleView() {
			p.Log.Warningf("Maximum staleness of %v exceeded, failure started at %v.", p.MaxStaleness, p.failureStartedAt)
			p.collector.notifyStale()
		}

		// Used for testing that the watch routine has exited and is about
		// to be restarted.
		select {
		case p.ResetC <- p.retry.Duration():
		default:
		}

		startedWaiting := p.Clock.Now()
		select {
		case t := <-p.retry.After():
			p.Log.Debugf("Attempting to restart watch after waiting %v.", t.Sub(startedWaiting))
			p.retry.Inc()
		case <-p.ctx.Done():
			p.Log.Debug("Closed, returning from watch loop.")
			return
		}
		if err != nil {
			p.Log.Warningf("Restart watch on error: %v.", err)
		} else {
			p.Log.Debug("Triggering scheduled refetch.")
		}

	}
}

// watch monitors new resource updates, maintains a local view and broadcasts
// notifications to connected agents.
func (p *resourceWatcher) watch() error {
	watcher, err := p.Client.NewWatcher(p.ctx, types.Watch{
		Name:            p.Component,
		MetricComponent: p.Component,
		Kinds:           []types.WatchKind{{Kind: p.collector.resourceKind()}},
	})
	if err != nil {
		return trace.Wrap(err)
	}
	defer watcher.Close()
	refetchC := time.After(p.RefetchPeriod)

	// before fetch, make sure watcher is synced by receiving init event,
	// to avoid the scenario:
	// 1. Cache process:   w = NewWatcher()
	// 2. Cache process:   c.fetch()
	// 3. Backend process: addItem()
	// 4. Cache process:   <- w.Events()
	//
	// If there is a way that NewWatcher() on line 1 could
	// return without subscription established first,
	// Code line 3 could execute and line 4 could miss event,
	// wrapping up with out of sync replica.
	// To avoid this, before doing fetch,
	// cache process makes sure the connection is established
	// by receiving init event first.
	select {
	case <-watcher.Done():
		return trace.ConnectionProblem(watcher.Error(), "watcher is closed")
	case <-refetchC:
		return nil
	case <-p.ctx.Done():
		return trace.ConnectionProblem(p.ctx.Err(), "context is closing")
	case event := <-watcher.Events():
		if event.Type != types.OpInit {
			return trace.BadParameter("expected init event, got %v instead", event.Type)
		}
	}

	if err := p.collector.getResourcesAndUpdateCurrent(p.ctx); err != nil {
		return trace.Wrap(err)
	}
	p.retry.Reset()
	p.failureStartedAt = time.Time{}

	for {
		select {
		case <-watcher.Done():
			return trace.ConnectionProblem(watcher.Error(), "watcher is closed")
		case <-refetchC:
			return nil
		case <-p.ctx.Done():
			return trace.ConnectionProblem(p.ctx.Err(), "context is closing")
		case event := <-watcher.Events():
			p.collector.processEventAndUpdateCurrent(p.ctx, event)
		case p.LoopC <- struct{}{}:
			// Used in tests to detect the watch loop is running.
		}
	}
}

// ProxyWatcherConfig is a ProxyWatcher configuration.
type ProxyWatcherConfig struct {
	ResourceWatcherConfig
	// ProxyGetter is used to directly fetch the list of active proxies.
	ProxyGetter
	// ProxiesC is a channel used to report the current proxy set. It receives
	// a fresh list at startup and subsequently a list of all known proxies
	// whenever an addition or deletion is detected.
	ProxiesC chan []types.Server
}

// CheckAndSetDefaults checks parameters and sets default values.
func (cfg *ProxyWatcherConfig) CheckAndSetDefaults() error {
	if err := cfg.ResourceWatcherConfig.CheckAndSetDefaults(); err != nil {
		return trace.Wrap(err)
	}
	if cfg.ProxyGetter == nil {
		getter, ok := cfg.Client.(ProxyGetter)
		if !ok {
			return trace.BadParameter("missing parameter ProxyGetter and Client not usable as ProxyGetter")
		}
		cfg.ProxyGetter = getter
	}
	if cfg.ProxiesC == nil {
		cfg.ProxiesC = make(chan []types.Server)
	}
	return nil
}

// NewProxyWatcher returns a new instance of ProxyWatcher.
func NewProxyWatcher(ctx context.Context, cfg ProxyWatcherConfig) (*ProxyWatcher, error) {
	if err := cfg.CheckAndSetDefaults(); err != nil {
		return nil, trace.Wrap(err)
	}
	collector := &proxyCollector{
		ProxyWatcherConfig: cfg,
	}
	watcher, err := newResourceWatcher(ctx, collector, cfg.ResourceWatcherConfig)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return &ProxyWatcher{watcher, collector}, nil
}

// ProxyWatcher is built on top of resourceWatcher to monitor additions
// and deletions to the set of proxies.
type ProxyWatcher struct {
	*resourceWatcher
	*proxyCollector
}

// proxyCollector accompanies resourceWatcher when monitoring proxies.
type proxyCollector struct {
	ProxyWatcherConfig
	// current holds a map of the currently known proxies (keyed by server name,
	// RWMutex protected).
	current map[string]types.Server
	rw      sync.RWMutex
}

// GetCurrent returns the currently stored proxies.
func (p *proxyCollector) GetCurrent() []types.Server {
	p.rw.RLock()
	defer p.rw.RUnlock()
	return serverMapValues(p.current)
}

// resourceKind specifies the resource kind to watch.
func (p *proxyCollector) resourceKind() string {
	return types.KindProxy
}

// getResourcesAndUpdateCurrent is called when the resources should be
// (re-)fetched directly.
func (p *proxyCollector) getResourcesAndUpdateCurrent(ctx context.Context) error {
	proxies, err := p.ProxyGetter.GetProxies()
	if err != nil {
		return trace.Wrap(err)
	}
	if len(proxies) == 0 {
		// At least one proxy ought to exist.
		return trace.NotFound("empty proxy list")
	}
	newCurrent := make(map[string]types.Server, len(proxies))
	for _, proxy := range proxies {
		newCurrent[proxy.GetName()] = proxy
	}
	p.rw.Lock()
	defer p.rw.Unlock()
	p.current = newCurrent
	p.broadcastUpdate(ctx)
	return nil
}

// processEventAndUpdateCurrent is called when a watcher event is received.
func (p *proxyCollector) processEventAndUpdateCurrent(ctx context.Context, event types.Event) {
	if event.Resource == nil || event.Resource.GetKind() != types.KindProxy {
		p.Log.Warningf("Unexpected event: %v.", event)
		return
	}

	p.rw.Lock()
	defer p.rw.Unlock()

	switch event.Type {
	case types.OpDelete:
		delete(p.current, event.Resource.GetName())
		// Always broadcast when a proxy is deleted.
		p.broadcastUpdate(ctx)
	case types.OpPut:
		server, ok := event.Resource.(types.Server)
		if !ok {
			p.Log.Warningf("Unexpected type %T.", event.Resource)
			return
		}
		_, known := p.current[server.GetName()]
		p.current[server.GetName()] = server
		// Broadcast only creation of new proxies (not known before).
		if !known {
			p.broadcastUpdate(ctx)
		}
	default:
		p.Log.Warningf("Skipping unsupported event type %s.", event.Type)
	}
}

// broadcastUpdate broadcasts information about updating the proxy set.
func (p *proxyCollector) broadcastUpdate(ctx context.Context) {
	names := make([]string, 0, len(p.current))
	for k := range p.current {
		names = append(names, k)
	}
	p.Log.Debugf("List of known proxies updated: %q.", names)

	select {
	case p.ProxiesC <- serverMapValues(p.current):
	case <-ctx.Done():
	}
}

func (p *proxyCollector) notifyStale() {}

func serverMapValues(serverMap map[string]types.Server) []types.Server {
	servers := make([]types.Server, 0, len(serverMap))
	for _, server := range serverMap {
		servers = append(servers, server)
	}
	return servers
}

// LockWatcherConfig is a LockWatcher configuration.
type LockWatcherConfig struct {
	ResourceWatcherConfig
	LockGetter
}

// CheckAndSetDefaults checks parameters and sets default values.
func (cfg *LockWatcherConfig) CheckAndSetDefaults() error {
	if err := cfg.ResourceWatcherConfig.CheckAndSetDefaults(); err != nil {
		return trace.Wrap(err)
	}
	if cfg.MaxStaleness == 0 {
		cfg.MaxStaleness = defaults.LockMaxStaleness
	}
	if cfg.LockGetter == nil {
		getter, ok := cfg.Client.(LockGetter)
		if !ok {
			return trace.BadParameter("missing parameter LockGetter and Client not usable as LockGetter")
		}
		cfg.LockGetter = getter
	}
	return nil
}

// NewLockWatcher returns a new instance of LockWatcher.
func NewLockWatcher(ctx context.Context, cfg LockWatcherConfig) (*LockWatcher, error) {
	if err := cfg.CheckAndSetDefaults(); err != nil {
		return nil, trace.Wrap(err)
	}
	collector := &lockCollector{
		LockWatcherConfig: cfg,
		fanout:            NewFanout(),
	}
	watcher, err := newResourceWatcher(ctx, collector, cfg.ResourceWatcherConfig)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	collector.fanout.SetInit()
	return &LockWatcher{watcher, collector}, nil
}

// LockWatcher is built on top of resourceWatcher to monitor changes to locks.
type LockWatcher struct {
	*resourceWatcher
	*lockCollector
}

// lockCollector accompanies resourceWatcher when monitoring locks.
type lockCollector struct {
	LockWatcherConfig
	// current holds a map of the currently known locks (keyed by lock name).
	current map[string]types.Lock
	// isStale indicates whether the local lock view (current) is stale.
	isStale bool
	// currentRW is a mutex protecting both current and isStale.
	currentRW sync.RWMutex
	// fanout provides support for multiple subscribers to the lock updates.
	fanout *Fanout
}

// Subscribe is used to subscribe to the lock updates.
func (p *lockCollector) Subscribe(ctx context.Context, targets ...types.LockTarget) (types.Watcher, error) {
	watchKinds, err := lockTargetsToWatchKinds(targets)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	sub, err := p.fanout.NewWatcher(ctx, types.Watch{Kinds: watchKinds})
	if err != nil {
		return nil, trace.Wrap(err)
	}
	select {
	case event := <-sub.Events():
		if event.Type != types.OpInit {
			return nil, trace.BadParameter("expected init event, got %v instead", event.Type)
		}
	case <-sub.Done():
		return nil, trace.Wrap(sub.Error())
	}
	return sub, nil
}

// CheckLockInForce returns an AccessDenied error if there is a lock in force
// matching at at least one of the targets.
func (p *lockCollector) CheckLockInForce(mode constants.LockingMode, targets ...types.LockTarget) error {
	p.currentRW.RLock()
	defer p.currentRW.RUnlock()
	if p.isStale && mode == constants.LockingModeStrict {
		return StrictLockingModeAccessDenied
	}
	if lock := p.findLockInForceUnderMutex(targets); lock != nil {
		return LockInForceAccessDenied(lock)
	}
	return nil
}

func (p *lockCollector) findLockInForceUnderMutex(targets []types.LockTarget) types.Lock {
	for _, lock := range p.current {
		if !lock.IsInForce(p.Clock.Now()) {
			continue
		}
		if len(targets) == 0 {
			return lock
		}
		for _, target := range targets {
			if target.Match(lock) {
				return lock
			}
		}
	}
	return nil
}

// GetCurrent returns the currently stored locks.
func (p *lockCollector) GetCurrent() []types.Lock {
	p.currentRW.RLock()
	defer p.currentRW.RUnlock()
	return lockMapValues(p.current)
}

// resourceKind specifies the resource kind to watch.
func (p *lockCollector) resourceKind() string {
	return types.KindLock
}

// getResourcesAndUpdateCurrent is called when the resources should be
// (re-)fetched directly.
func (p *lockCollector) getResourcesAndUpdateCurrent(ctx context.Context) error {
	locks, err := p.LockGetter.GetLocks(ctx, true)
	if err != nil {
		return trace.Wrap(err)
	}
	newCurrent := map[string]types.Lock{}
	for _, lock := range locks {
		newCurrent[lock.GetName()] = lock
	}

	p.currentRW.Lock()
	defer p.currentRW.Unlock()
	p.current = newCurrent
	p.isStale = false
	for _, lock := range p.current {
		p.fanout.Emit(types.Event{Type: types.OpPut, Resource: lock})
	}
	return nil
}

// processEventAndUpdateCurrent is called when a watcher event is received.
func (p *lockCollector) processEventAndUpdateCurrent(ctx context.Context, event types.Event) {
	if event.Resource == nil || event.Resource.GetKind() != types.KindLock {
		p.Log.Warningf("Unexpected event: %v.", event)
		return
	}

	p.currentRW.Lock()
	defer p.currentRW.Unlock()
	switch event.Type {
	case types.OpDelete:
		delete(p.current, event.Resource.GetName())
		p.fanout.Emit(event)
	case types.OpPut:
		lock, ok := event.Resource.(types.Lock)
		if !ok {
			p.Log.Warningf("Unexpected resource type %T.", event.Resource)
			return
		}
		if lock.IsInForce(p.Clock.Now()) {
			p.current[lock.GetName()] = lock
			p.fanout.Emit(event)
		} else {
			delete(p.current, lock.GetName())
		}
	default:
		p.Log.Warningf("Skipping unsupported event type %s.", event.Type)
	}
}

// notifyStale is called when the maximum acceptable staleness (if specified)
// is exceeded.
func (p *lockCollector) notifyStale() {
	p.currentRW.Lock()
	defer p.currentRW.Unlock()

	p.fanout.Emit(types.Event{Type: types.OpUnreliable})

	// Do not clear p.current here, the most recent lock set may still be used
	// with LockingModeBestEffort.
	p.isStale = true
}

func lockTargetsToWatchKinds(targets []types.LockTarget) ([]types.WatchKind, error) {
	watchKinds := make([]types.WatchKind, 0, len(targets))
	for _, target := range targets {
		if target.IsEmpty() {
			continue
		}
		filter, err := target.IntoMap()
		if err != nil {
			return nil, trace.Wrap(err)
		}
		watchKinds = append(watchKinds, types.WatchKind{
			Kind:   types.KindLock,
			Filter: filter,
		})
	}
	if len(watchKinds) == 0 {
		watchKinds = []types.WatchKind{{Kind: types.KindLock}}
	}
	return watchKinds, nil
}

func lockMapValues(lockMap map[string]types.Lock) []types.Lock {
	locks := make([]types.Lock, 0, len(lockMap))
	for _, lock := range lockMap {
		locks = append(locks, lock)
	}
	return locks
}

// DatabaseWatcherConfig is a DatabaseWatcher configuration.
type DatabaseWatcherConfig struct {
	// ResourceWatcherConfig is the resource watcher configuration.
	ResourceWatcherConfig
	// DatabaseGetter is responsible for fetching database resources.
	DatabaseGetter
	// DatabasesC receives up-to-date list of all database resources.
	DatabasesC chan types.Databases
}

// CheckAndSetDefaults checks parameters and sets default values.
func (cfg *DatabaseWatcherConfig) CheckAndSetDefaults() error {
	if err := cfg.ResourceWatcherConfig.CheckAndSetDefaults(); err != nil {
		return trace.Wrap(err)
	}
	if cfg.DatabaseGetter == nil {
		getter, ok := cfg.Client.(DatabaseGetter)
		if !ok {
			return trace.BadParameter("missing parameter DatabaseGetter and Client not usable as DatabaseGetter")
		}
		cfg.DatabaseGetter = getter
	}
	if cfg.DatabasesC == nil {
		cfg.DatabasesC = make(chan types.Databases)
	}
	return nil
}

// NewDatabaseWatcher returns a new instance of DatabaseWatcher.
func NewDatabaseWatcher(ctx context.Context, cfg DatabaseWatcherConfig) (*DatabaseWatcher, error) {
	if err := cfg.CheckAndSetDefaults(); err != nil {
		return nil, trace.Wrap(err)
	}
	collector := &databaseCollector{
		DatabaseWatcherConfig: cfg,
	}
	watcher, err := newResourceWatcher(ctx, collector, cfg.ResourceWatcherConfig)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return &DatabaseWatcher{watcher, collector}, nil
}

// DatabaseWatcher is built on top of resourceWatcher to monitor database resources.
type DatabaseWatcher struct {
	*resourceWatcher
	*databaseCollector
}

// databaseCollector accompanies resourceWatcher when monitoring database resources.
type databaseCollector struct {
	// DatabaseWatcherConfig is the watcher configuration.
	DatabaseWatcherConfig
	// current holds a map of the currently known database resources.
	current map[string]types.Database
	// lock protects the "current" map.
	lock sync.RWMutex
}

// resourceKind specifies the resource kind to watch.
func (p *databaseCollector) resourceKind() string {
	return types.KindDatabase
}

// getResourcesAndUpdateCurrent refreshes the list of current resources.
func (p *databaseCollector) getResourcesAndUpdateCurrent(ctx context.Context) error {
	databases, err := p.DatabaseGetter.GetDatabases(ctx)
	if err != nil {
		return trace.Wrap(err)
	}
	newCurrent := make(map[string]types.Database, len(databases))
	for _, database := range databases {
		newCurrent[database.GetName()] = database
	}
	p.lock.Lock()
	defer p.lock.Unlock()
	p.current = newCurrent

	select {
	case <-ctx.Done():
		return trace.Wrap(ctx.Err())
	case p.DatabasesC <- databases:
	}

	return nil
}

// processEventAndUpdateCurrent is called when a watcher event is received.
func (p *databaseCollector) processEventAndUpdateCurrent(ctx context.Context, event types.Event) {
	if event.Resource == nil || event.Resource.GetKind() != types.KindDatabase {
		p.Log.Warnf("Unexpected event: %v.", event)
		return
	}
	p.lock.Lock()
	defer p.lock.Unlock()
	switch event.Type {
	case types.OpDelete:
		delete(p.current, event.Resource.GetName())
		select {
		case <-ctx.Done():
		case p.DatabasesC <- databasesToSlice(p.current):
		}
	case types.OpPut:
		database, ok := event.Resource.(types.Database)
		if !ok {
			p.Log.Warnf("Unexpected resource type %T.", event.Resource)
			return
		}
		p.current[database.GetName()] = database
		select {
		case <-ctx.Done():
		case p.DatabasesC <- databasesToSlice(p.current):
		}

	default:
		p.Log.Warnf("Unsupported event type %s.", event.Type)
		return
	}
}

func (*databaseCollector) notifyStale() {}

func databasesToSlice(databases map[string]types.Database) (slice []types.Database) {
	for _, database := range databases {
		slice = append(slice, database)
	}
	return slice
}

// AppWatcherConfig is an AppWatcher configuration.
type AppWatcherConfig struct {
	// ResourceWatcherConfig is the resource watcher configuration.
	ResourceWatcherConfig
	// AppGetter is responsible for fetching application resources.
	AppGetter
	// AppsC receives up-to-date list of all application resources.
	AppsC chan types.Apps
}

// CheckAndSetDefaults checks parameters and sets default values.
func (cfg *AppWatcherConfig) CheckAndSetDefaults() error {
	if err := cfg.ResourceWatcherConfig.CheckAndSetDefaults(); err != nil {
		return trace.Wrap(err)
	}
	if cfg.AppGetter == nil {
		getter, ok := cfg.Client.(AppGetter)
		if !ok {
			return trace.BadParameter("missing parameter AppGetter and Client not usable as AppGetter")
		}
		cfg.AppGetter = getter
	}
	if cfg.AppsC == nil {
		cfg.AppsC = make(chan types.Apps)
	}
	return nil
}

// NewAppWatcher returns a new instance of AppWatcher.
func NewAppWatcher(ctx context.Context, cfg AppWatcherConfig) (*AppWatcher, error) {
	if err := cfg.CheckAndSetDefaults(); err != nil {
		return nil, trace.Wrap(err)
	}
	collector := &appCollector{
		AppWatcherConfig: cfg,
	}
	watcher, err := newResourceWatcher(ctx, collector, cfg.ResourceWatcherConfig)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return &AppWatcher{watcher, collector}, nil
}

// AppWatcher is built on top of resourceWatcher to monitor application resources.
type AppWatcher struct {
	*resourceWatcher
	*appCollector
}

// appCollector accompanies resourceWatcher when monitoring application resources.
type appCollector struct {
	// AppWatcherConfig is the watcher configuration.
	AppWatcherConfig
	// current holds a map of the currently known application resources.
	current map[string]types.Application
	// lock protects the "current" map.
	lock sync.RWMutex
}

// resourceKind specifies the resource kind to watch.
func (p *appCollector) resourceKind() string {
	return types.KindApp
}

// getResourcesAndUpdateCurrent refreshes the list of current resources.
func (p *appCollector) getResourcesAndUpdateCurrent(ctx context.Context) error {
	apps, err := p.AppGetter.GetApps(ctx)
	if err != nil {
		return trace.Wrap(err)
	}
	newCurrent := make(map[string]types.Application, len(apps))
	for _, app := range apps {
		newCurrent[app.GetName()] = app
	}
	p.lock.Lock()
	defer p.lock.Unlock()
	p.current = newCurrent

	select {
	case <-ctx.Done():
		return trace.Wrap(ctx.Err())
	case p.AppsC <- apps:
	}
	return nil
}

// processEventAndUpdateCurrent is called when a watcher event is received.
func (p *appCollector) processEventAndUpdateCurrent(ctx context.Context, event types.Event) {
	if event.Resource == nil || event.Resource.GetKind() != types.KindApp {
		p.Log.Warnf("Unexpected event: %v.", event)
		return
	}
	p.lock.Lock()
	defer p.lock.Unlock()
	switch event.Type {
	case types.OpDelete:
		delete(p.current, event.Resource.GetName())
		p.AppsC <- appsToSlice(p.current)

		select {
		case <-ctx.Done():
		case p.AppsC <- appsToSlice(p.current):
		}

	case types.OpPut:
		app, ok := event.Resource.(types.Application)
		if !ok {
			p.Log.Warnf("Unexpected resource type %T.", event.Resource)
			return
		}
		p.current[app.GetName()] = app

		select {
		case <-ctx.Done():
		case p.AppsC <- appsToSlice(p.current):
		}
	default:
		p.Log.Warnf("Unsupported event type %s.", event.Type)
		return
	}
}

func (*appCollector) notifyStale() {}

func appsToSlice(apps map[string]types.Application) (slice []types.Application) {
	for _, app := range apps {
		slice = append(slice, app)
	}
	return slice
}

// CertAuthorityWatcherConfig is a CertAuthorityWatcher configuration.
type CertAuthorityWatcherConfig struct {
	// ResourceWatcherConfig is the resource watcher configuration.
	ResourceWatcherConfig
	// AuthorityGetter is responsible for fetching cert authority resources.
	AuthorityGetter
	// Types restricts which cert authority types are retrieved via the AuthorityGetter.
	Types []types.CertAuthType
}

// CheckAndSetDefaults checks parameters and sets default values.
func (cfg *CertAuthorityWatcherConfig) CheckAndSetDefaults() error {
	if err := cfg.ResourceWatcherConfig.CheckAndSetDefaults(); err != nil {
		return trace.Wrap(err)
	}
	if cfg.AuthorityGetter == nil {
		getter, ok := cfg.Client.(AuthorityGetter)
		if !ok {
			return trace.BadParameter("missing parameter AuthorityGetter and Client not usable as AuthorityGetter")
		}
		cfg.AuthorityGetter = getter
	}
	return nil
}

// IsWatched return true if the given certificate auth type is being observer by the watcher.
func (cfg *CertAuthorityWatcherConfig) IsWatched(certType types.CertAuthType) bool {
	for _, observedType := range cfg.Types {
		if observedType == certType {
			return true
		}
	}
	return false
}

// NewCertAuthorityWatcher returns a new instance of CertAuthorityWatcher.
func NewCertAuthorityWatcher(ctx context.Context, cfg CertAuthorityWatcherConfig) (*CertAuthorityWatcher, error) {
	if err := cfg.CheckAndSetDefaults(); err != nil {
		return nil, trace.Wrap(err)
	}

	collector := &caCollector{
		CertAuthorityWatcherConfig: cfg,
		fanout:                     NewFanout(),
		cas:                        make(map[types.CertAuthType]map[string]types.CertAuthority, len(cfg.Types)),
	}

	for _, t := range cfg.Types {
		collector.cas[t] = make(map[string]types.CertAuthority)
	}

	watcher, err := newResourceWatcher(ctx, collector, cfg.ResourceWatcherConfig)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	collector.fanout.SetInit()
	return &CertAuthorityWatcher{watcher, collector}, nil
}

// CertAuthorityWatcher is built on top of resourceWatcher to monitor cert authority resources.
type CertAuthorityWatcher struct {
	*resourceWatcher
	*caCollector
}

// caCollector accompanies resourceWatcher when monitoring cert authority resources.
type caCollector struct {
	CertAuthorityWatcherConfig
	fanout *Fanout

	// lock protects concurrent access to cas
	lock sync.RWMutex
	// cas maps ca type -> cluster -> ca
	cas map[types.CertAuthType]map[string]types.CertAuthority
}

// CertAuthorityTarget lists the attributes of interactions to be disabled.
type CertAuthorityTarget struct {
	// ClusterName specifies the name of the cluster to watch.
	ClusterName string
	// Type specifies the ca types to watch for.
	Type types.CertAuthType
}

// Subscribe is used to subscribe to the lock updates.
func (c *caCollector) Subscribe(ctx context.Context, targets ...CertAuthorityTarget) (types.Watcher, error) {
	watchKinds, err := caTargetToWatchKinds(targets)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	sub, err := c.fanout.NewWatcher(ctx, types.Watch{Kinds: watchKinds})
	if err != nil {
		return nil, trace.Wrap(err)
	}
	select {
	case event := <-sub.Events():
		if event.Type != types.OpInit {
			return nil, trace.BadParameter("expected init event, got %v instead", event.Type)
		}
	case <-sub.Done():
		return nil, trace.Wrap(sub.Error())
	}
	return sub, nil
}

func caTargetToWatchKinds(targets []CertAuthorityTarget) ([]types.WatchKind, error) {
	watchKinds := make([]types.WatchKind, 0, len(targets))
	for _, target := range targets {
		kind := types.WatchKind{
			Kind: types.KindCertAuthority,
			// Note that watching SubKind doesn't work for types.WatchKind - to do so it would
			// require a custom filter, which was recently added but - we can't use yet due to
			// older clients not supporting the filter.
			SubKind: string(target.Type),
		}

		if target.ClusterName != "" {
			kind.Name = target.ClusterName
		}

		watchKinds = append(watchKinds, kind)
	}

	if len(watchKinds) == 0 {
		watchKinds = []types.WatchKind{{Kind: types.KindCertAuthority}}
	}

	return watchKinds, nil
}

// resourceKind specifies the resource kind to watch.
func (c *caCollector) resourceKind() string {
	return types.KindCertAuthority
}

// getResourcesAndUpdateCurrent refreshes the list of current resources.
func (c *caCollector) getResourcesAndUpdateCurrent(ctx context.Context) error {
	var cas []types.CertAuthority

	for _, t := range c.Types {
		authorities, err := c.AuthorityGetter.GetCertAuthorities(ctx, t, false)
		if err != nil {
			return trace.Wrap(err)
		}

		cas = append(cas, authorities...)
	}

	c.lock.Lock()
	defer c.lock.Unlock()

	for _, ca := range cas {
		if !c.watchingType(ca.GetType()) {
			continue
		}

		c.cas[ca.GetType()][ca.GetName()] = ca
		c.fanout.Emit(types.Event{Type: types.OpPut, Resource: ca.Clone()})
	}
	return nil
}

// processEventAndUpdateCurrent is called when a watcher event is received.
func (c *caCollector) processEventAndUpdateCurrent(ctx context.Context, event types.Event) {
	if event.Resource == nil || event.Resource.GetKind() != types.KindCertAuthority {
		c.Log.Warnf("Unexpected event: %v.", event)
		return
	}
	c.lock.Lock()
	defer c.lock.Unlock()
	switch event.Type {
	case types.OpDelete:
		caType := types.CertAuthType(event.Resource.GetSubKind())
		if !c.watchingType(caType) {
			return
		}

		delete(c.cas[caType], event.Resource.GetName())
		c.fanout.Emit(event)
	case types.OpPut:
		ca, ok := event.Resource.(types.CertAuthority)
		if !ok {
			c.Log.Warnf("Unexpected resource type %T.", event.Resource)
			return
		}

		if !c.watchingType(ca.GetType()) {
			return
		}

		authority, ok := c.cas[ca.GetType()][ca.GetName()]
		if ok && CertAuthoritiesEquivalent(authority, ca) {
			return
		}

		c.cas[ca.GetType()][ca.GetName()] = ca
		c.fanout.Emit(event)
	default:
		c.Log.Warnf("Unsupported event type %s.", event.Type)
		return
	}
}

func (c *caCollector) watchingType(t types.CertAuthType) bool {
	for _, caType := range c.Types {
		if caType == t {
			return true
		}
	}

	return false
}

func (c *caCollector) notifyStale() {}

// NodeWatcherConfig is a NodeWatcher configuration.
type NodeWatcherConfig struct {
	ResourceWatcherConfig
	// NodesGetter is used to directly fetch the list of active nodes.
	NodesGetter
}

// CheckAndSetDefaults checks parameters and sets default values.
func (cfg *NodeWatcherConfig) CheckAndSetDefaults() error {
	if err := cfg.ResourceWatcherConfig.CheckAndSetDefaults(); err != nil {
		return trace.Wrap(err)
	}
	if cfg.NodesGetter == nil {
		getter, ok := cfg.Client.(NodesGetter)
		if !ok {
			return trace.BadParameter("missing parameter NodesGetter and Client not usable as NodesGetter")
		}
		cfg.NodesGetter = getter
	}
	return nil
}

// NewNodeWatcher returns a new instance of NodeWatcher.
func NewNodeWatcher(ctx context.Context, cfg NodeWatcherConfig) (*NodeWatcher, error) {
	if err := cfg.CheckAndSetDefaults(); err != nil {
		return nil, trace.Wrap(err)
	}
	collector := &nodeCollector{
		NodeWatcherConfig: cfg,
		current:           map[string]types.Server{},
	}
	watcher, err := newResourceWatcher(ctx, collector, cfg.ResourceWatcherConfig)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return &NodeWatcher{watcher, collector}, nil
}

// NodeWatcher is built on top of resourceWatcher to monitor additions
// and deletions to the set of nodes.
type NodeWatcher struct {
	*resourceWatcher
	*nodeCollector
}

// nodeCollector accompanies resourceWatcher when monitoring nodes.
type nodeCollector struct {
	NodeWatcherConfig
	// current holds a map of the currently known nodes (keyed by server name,
	// RWMutex protected).
	current map[string]types.Server
	rw      sync.RWMutex
}

// Node is a readonly subset of the types.Server interface which
// users may filter by in GetNodes.
type Node interface {
	// ResourceWithLabels provides common resource headers
	types.ResourceWithLabels
	// GetTeleportVersion returns the teleport version the server is running on
	GetTeleportVersion() string
	// GetAddr return server address
	GetAddr() string
	// GetHostname returns server hostname
	GetHostname() string
	// GetNamespace returns server namespace
	GetNamespace() string
	// GetLabels returns server's static label key pairs
	GetLabels() map[string]string
	// GetCmdLabels gets command labels
	GetCmdLabels() map[string]types.CommandLabel
	// GetPublicAddr is an optional field that returns the public address this cluster can be reached at.
	GetPublicAddr() string
	// GetRotation gets the state of certificate authority rotation.
	GetRotation() types.Rotation
	// GetUseTunnel gets if a reverse tunnel should be used to connect to this node.
	GetUseTunnel() bool
}

// GetNodes allows callers to retrieve a subset of nodes that match the filter provided. The
// returned servers are a copy and can be safely modified. It is intentionally hard to retrieve
// the full set of nodes to reduce the number of copies needed since the number of nodes can get
// quite large and doing so can be expensive.
func (n *nodeCollector) GetNodes(fn func(n Node) bool) []types.Server {
	n.rw.RLock()
	defer n.rw.RUnlock()

	var matched []types.Server
	for _, server := range n.current {
		if fn(server) {
			matched = append(matched, server.DeepCopy())
		}
	}

	return matched
}

func (n *nodeCollector) NodeCount() int {
	n.rw.RLock()
	defer n.rw.RUnlock()
	return len(n.current)
}

// resourceKind specifies the resource kind to watch.
func (n *nodeCollector) resourceKind() string {
	return types.KindNode
}

// getResourcesAndUpdateCurrent is called when the resources should be
// (re-)fetched directly.
func (n *nodeCollector) getResourcesAndUpdateCurrent(ctx context.Context) error {
	nodes, err := n.NodesGetter.GetNodes(ctx, apidefaults.Namespace)
	if err != nil {
		return trace.Wrap(err)
	}
	if len(nodes) == 0 {
		return nil
	}
	newCurrent := make(map[string]types.Server, len(nodes))
	for _, node := range nodes {
		newCurrent[node.GetName()] = node
	}
	n.rw.Lock()
	defer n.rw.Unlock()
	n.current = newCurrent
	return nil
}

// processEventAndUpdateCurrent is called when a watcher event is received.
func (n *nodeCollector) processEventAndUpdateCurrent(ctx context.Context, event types.Event) {
	if event.Resource == nil || event.Resource.GetKind() != types.KindNode {
		n.Log.Warningf("Unexpected event: %v.", event)
		return
	}

	n.rw.Lock()
	defer n.rw.Unlock()

	switch event.Type {
	case types.OpDelete:
		delete(n.current, event.Resource.GetName())
	case types.OpPut:
		server, ok := event.Resource.(types.Server)
		if !ok {
			n.Log.Warningf("Unexpected type %T.", event.Resource)
			return
		}
		n.current[server.GetName()] = server
	default:
		n.Log.Warningf("Skipping unsupported event type %s.", event.Type)
	}
}

func (n *nodeCollector) notifyStale() {}
