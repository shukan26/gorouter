package registry

import (
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/uber-go/zap"

	"code.cloudfoundry.org/gorouter/config"
	"code.cloudfoundry.org/gorouter/logger"
	"code.cloudfoundry.org/gorouter/metrics"
	"code.cloudfoundry.org/gorouter/registry/container"
	"code.cloudfoundry.org/gorouter/route"
)

//go:generate counterfeiter -o fakes/fake_registry.go . Registry
type Registry interface {
	Register(uri route.Uri, endpoint *route.Endpoint)
	Unregister(uri route.Uri, endpoint *route.Endpoint)
	Lookup(uri route.Uri) *route.Pool
	LookupWithInstance(uri route.Uri, appID, appIndex string) *route.Pool
	StartPruningCycle()
	StopPruningCycle()
	NumUris() int
	NumEndpoints() int
	MarshalJSON() ([]byte, error)
}

type PruneStatus int

const (
	CONNECTED = PruneStatus(iota)
	DISCONNECTED
)

type RouteRegistry struct {
	sync.RWMutex

	logger logger.Logger

	// Access to the Trie datastructure should be governed by the RWMutex of RouteRegistry
	byURI *container.Trie

	// used for ability to suspend pruning
	suspendPruning func() bool
	pruningStatus  PruneStatus

	pruneStaleDropletsInterval time.Duration
	dropletStaleThreshold      time.Duration

	reporter metrics.RouteRegistryReporter

	ticker           *time.Ticker
	timeOfLastUpdate time.Time

	routerGroupGUID string
}

func NewRouteRegistry(logger logger.Logger, c *config.Config, reporter metrics.RouteRegistryReporter, routerGroupGUID string) *RouteRegistry {
	r := &RouteRegistry{}
	r.logger = logger
	r.byURI = container.NewTrie()

	r.pruneStaleDropletsInterval = c.PruneStaleDropletsInterval
	r.dropletStaleThreshold = c.DropletStaleThreshold
	r.suspendPruning = func() bool { return false }

	r.reporter = reporter
	r.routerGroupGUID = routerGroupGUID
	return r
}

func (r *RouteRegistry) Register(uri route.Uri, endpoint *route.Endpoint) {
	t := time.Now()

	r.Lock()

	routekey := uri.RouteKey()

	pool := r.byURI.Find(routekey)
	if pool == nil {
		contextPath := parseContextPath(uri)
		pool = route.NewPool(r.dropletStaleThreshold/4, contextPath)
		r.byURI.Insert(routekey, pool)
		r.logger.Debug("uri-added", zap.Stringer("uri", routekey))
	}

	endpointAdded := pool.Put(endpoint)

	r.timeOfLastUpdate = t
	r.Unlock()

	r.reporter.CaptureRegistryMessage(endpoint)

	routerGroupGUID := r.routerGroupGUID
	if routerGroupGUID == "" {
		routerGroupGUID = "-"
	}

	zapData := []zap.Field{
		zap.Stringer("uri", uri),
		zap.String("router-group-guid", routerGroupGUID),
		zap.String("backend", endpoint.CanonicalAddr()),
		zap.Object("modification_tag", endpoint.ModificationTag),
	}

	if endpointAdded {
		r.logger.Debug("endpoint-registered", zapData...)
	} else {
		r.logger.Debug("endpoint-not-registered", zapData...)
	}
}

func (r *RouteRegistry) Unregister(uri route.Uri, endpoint *route.Endpoint) {
	routerGroupGUID := r.routerGroupGUID
	if routerGroupGUID == "" {
		routerGroupGUID = "-"
	}

	zapData := []zap.Field{
		zap.Stringer("uri", uri),
		zap.String("router-group-guid", routerGroupGUID),
		zap.String("backend", endpoint.CanonicalAddr()),
		zap.Object("modification_tag", endpoint.ModificationTag),
	}

	r.Lock()

	uri = uri.RouteKey()

	pool := r.byURI.Find(uri)
	if pool != nil {
		endpointRemoved := pool.Remove(endpoint)
		if endpointRemoved {
			r.logger.Debug("endpoint-unregistered", zapData...)
		} else {
			r.logger.Debug("endpoint-not-unregistered", zapData...)
		}

		if pool.IsEmpty() {
			r.byURI.Delete(uri)
		}
	}

	r.Unlock()
	r.reporter.CaptureUnregistryMessage(endpoint)
}

func (r *RouteRegistry) Lookup(uri route.Uri) *route.Pool {
	started := time.Now()

	r.RLock()

	uri = uri.RouteKey()
	var err error
	pool := r.byURI.MatchUri(uri)
	for pool == nil && err == nil {
		uri, err = uri.NextWildcard()
		pool = r.byURI.MatchUri(uri)
	}

	r.RUnlock()
	endLookup := time.Now()
	r.reporter.CaptureLookupTime(endLookup.Sub(started))
	return pool
}

func (r *RouteRegistry) LookupWithInstance(uri route.Uri, appID string, appIndex string) *route.Pool {
	uri = uri.RouteKey()
	p := r.Lookup(uri)

	if p == nil {
		return nil
	}

	var surgicalPool *route.Pool

	p.Each(func(e *route.Endpoint) {
		if (e.ApplicationId == appID) && (e.PrivateInstanceIndex == appIndex) {
			surgicalPool = route.NewPool(0, "")
			surgicalPool.Put(e)
		}
	})
	return surgicalPool
}

func (r *RouteRegistry) StartPruningCycle() {
	if r.pruneStaleDropletsInterval > 0 {
		r.Lock()
		r.ticker = time.NewTicker(r.pruneStaleDropletsInterval)
		r.Unlock()

		go func() {
			for {
				select {
				case <-r.ticker.C:
					r.logger.Info("start-pruning-routes")
					r.pruneStaleDroplets()
					r.logger.Info("finished-pruning-routes")
					msSinceLastUpdate := uint64(time.Since(r.TimeOfLastUpdate()) / time.Millisecond)
					r.reporter.CaptureRouteStats(r.NumUris(), msSinceLastUpdate)
				}
			}
		}()
	}
}

func (r *RouteRegistry) StopPruningCycle() {
	r.Lock()
	if r.ticker != nil {
		r.ticker.Stop()
	}
	r.Unlock()
}

func (registry *RouteRegistry) NumUris() int {
	registry.RLock()
	uriCount := registry.byURI.PoolCount()
	registry.RUnlock()

	return uriCount
}

func (r *RouteRegistry) TimeOfLastUpdate() time.Time {
	r.RLock()
	t := r.timeOfLastUpdate
	r.RUnlock()

	return t
}

func (r *RouteRegistry) NumEndpoints() int {
	r.RLock()
	count := r.byURI.EndpointCount()
	r.RUnlock()

	return count
}

func (r *RouteRegistry) MarshalJSON() ([]byte, error) {
	r.RLock()
	defer r.RUnlock()

	return json.Marshal(r.byURI.ToMap())
}

func (r *RouteRegistry) pruneStaleDroplets() {
	r.Lock()
	defer r.Unlock()

	// suspend pruning if option enabled and if NATS is unavailable
	if r.suspendPruning() {
		r.logger.Info("prune-suspended")
		r.pruningStatus = DISCONNECTED
		return
	}
	if r.pruningStatus == DISCONNECTED {
		// if we are coming back from being disconnected from source,
		// bulk update routes / mark updated to avoid pruning right away
		r.logger.Debug("prune-unsuspended-refresh-routes-start")
		r.freshenRoutes()
		r.logger.Debug("prune-unsuspended-refresh-routes-complete")
	}
	r.pruningStatus = CONNECTED

	routerGroupGUID := r.routerGroupGUID
	if routerGroupGUID == "" {
		routerGroupGUID = "-"
	}

	r.byURI.EachNodeWithPool(func(t *container.Trie) {
		endpoints := t.Pool.PruneEndpoints(r.dropletStaleThreshold)
		t.Snip()
		if len(endpoints) > 0 {
			addresses := []string{}
			for _, e := range endpoints {
				addresses = append(addresses, e.CanonicalAddr())
			}
			r.logger.Info("pruned-route",
				zap.String("uri", t.ToPath()),
				zap.Object("endpoints", addresses),
				zap.String("router-group-guid", routerGroupGUID),
			)
		}
	})
}

func (r *RouteRegistry) SuspendPruning(f func() bool) {
	r.Lock()
	r.suspendPruning = f
	r.Unlock()
}

// bulk update to mark pool / endpoints as updated
func (r *RouteRegistry) freshenRoutes() {
	now := time.Now()
	r.byURI.EachNodeWithPool(func(t *container.Trie) {
		t.Pool.MarkUpdated(now)
	})
}

func parseContextPath(uri route.Uri) string {
	contextPath := "/"
	split := strings.SplitN(strings.TrimPrefix(uri.String(), "/"), "/", 2)

	if len(split) > 1 {
		contextPath += split[1]
	}

	if idx := strings.Index(string(contextPath), "?"); idx >= 0 {
		contextPath = contextPath[0:idx]
	}

	return contextPath
}
