/*
Copyright 2025 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package datastore

import (
	"context"
	"errors"
	"sync"

	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/collector/source/pod"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/config"
	poolutil "github.com/llm-d-incubation/workload-variant-autoscaler/internal/utils/pool"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	errPoolNotSynced = errors.New("EndpointPool not found in datastore")
	errPoolIsNull    = errors.New("EndpointPool object is nil, does not exist")
)

// The datastore is a local cache of relevant data for the given InferencePool (currently all pulled from k8s-api)
type Datastore interface {
	// InferencePool operations
	PoolSet(ctx context.Context, client client.Client, pool *poolutil.EndpointPool) error
	PoolGet(name string) (*poolutil.EndpointPool, error)
	PoolGetMetricsSource(name string) source.MetricsSource
	PoolList() []*poolutil.EndpointPool
	PoolGetFromLabels(labels map[string]string) (*poolutil.EndpointPool, error)
	PoolDelete(name string)

	// Clears the store state, happens when the pool gets deleted.
	Clear()
}

func NewDatastore(cfg *config.Config) Datastore {
	store := &datastore{
		pools:    &sync.Map{},
		registry: source.NewSourceRegistry(),
		config:   cfg,
	}
	return store
}

type datastore struct {
	pools    *sync.Map
	registry *source.SourceRegistry
	config   *config.Config // Unified configuration (injected from main.go)
}

// Datastore operations
func (ds *datastore) PoolSet(ctx context.Context, client client.Client, pool *poolutil.EndpointPool) error {
	if pool == nil {
		return errPoolIsNull
	}

	if ds.registry.Get(pool.Name) == nil {
		// Create pod source
		var token string
		if ds.config != nil && ds.config.Static.EPPConfig != nil {
			token = ds.config.Static.EPPConfig.MetricReaderBearerToken
		}
		podConfig := pod.PodScrapingSourceConfig{
			ServiceName:      pool.EndpointPicker.ServiceName,
			ServiceNamespace: pool.EndpointPicker.Namespace,
			MetricsPort:      pool.EndpointPicker.MetricsPortNumber,
			BearerToken:      token,
		}

		podSource, err := pod.NewPodScrapingSource(ctx, client, podConfig)
		if err != nil {
			return err
		}

		// Register in registry
		// TODO: We need to be able to update or delete a pod source object in the registry at internal/collector/source/registry.go
		if err := ds.registry.Register(pool.Name, podSource); err != nil {
			return err
		}
	}

	// Store in the datastore
	ds.pools.Store(pool.Name, pool)
	return nil
}

func (ds *datastore) PoolGet(name string) (*poolutil.EndpointPool, error) {

	pool, exist := ds.pools.Load(name)
	if !exist {
		return nil, errPoolNotSynced
	}

	epp := pool.(*poolutil.EndpointPool)
	return epp, nil
}

func (ds *datastore) PoolGetMetricsSource(name string) source.MetricsSource {
	source := ds.registry.Get(name)
	return source
}

func (ds *datastore) PoolGetFromLabels(labels map[string]string) (*poolutil.EndpointPool, error) {
	exist := false
	var ep *poolutil.EndpointPool

	ds.pools.Range(func(k, v any) bool {
		ep = v.(*poolutil.EndpointPool)

		found := poolutil.IsSubset(ep.Selector, labels)
		if found {
			exist = true
			return false
		}
		return true
	})

	if !exist {
		return nil, errPoolNotSynced
	}
	return ep, nil
}

func (ds *datastore) PoolList() []*poolutil.EndpointPool {
	res := []*poolutil.EndpointPool{}
	ds.pools.Range(func(k, v any) bool {
		res = append(res, v.(*poolutil.EndpointPool))
		return true
	})

	return res
}

func (ds *datastore) PoolDelete(name string) {
	ds.pools.Delete(name)
}

func (ds *datastore) Clear() {
	ds.pools.Clear()
}
