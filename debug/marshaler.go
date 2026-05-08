package debug

import (
	"encoding/json"
	"slices"

	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	"github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"google.golang.org/protobuf/encoding/protojson"
)

type cacheMarshaler struct {
	cache.Cache
}

// minimal interface for reading data out of caches.
type snapshotReader interface {
	GetSnapshot(node string) (cache.ResourceSnapshot, error)
	GetStatusKeys() []string
}

func (c cacheMarshaler) MarshalJSON() ([]byte, error) {
	reader, ok := c.Cache.(snapshotReader)
	if !ok {
		return nil, nil
	}

	nodes := reader.GetStatusKeys()
	// Include the default snapshot as it can be setup before any subscriptions.
	if !slices.Contains(nodes, "") {
		nodes = append(nodes, "")
	}

	out := map[string]map[resource.Type]interface{}{}
	for _, node := range nodes {
		snapshot, err := reader.GetSnapshot(node)
		if err != nil {
			continue
		}

		nodeMap := map[resource.Type]interface{}{}
		for i := types.ResponseType(0); i < types.UnknownType; i++ {
			typeURL, _ := cache.GetResponseTypeURL(i)
			version := snapshot.GetVersion(typeURL)
			resources := snapshot.GetResources(typeURL)

			if len(resources) == 0 {
				continue
			}

			nodeMap[typeURL] = resourcesMarshaler{
				version:   version,
				resources: resources,
			}
		}
		if len(nodeMap) > 0 {
			out[node] = nodeMap
		}
	}

	return json.Marshal(out)
}

type resourcesMarshaler struct {
	version   string
	resources map[string]types.Resource
}

func (r resourcesMarshaler) MarshalJSON() ([]byte, error) {
	type outMap struct {
		Version string                       `json:"version"`
		Items   map[string]resourceMarshaler `json:"items"`
	}

	out := outMap{
		Version: r.version,
		Items:   map[string]resourceMarshaler{},
	}

	for k, v := range r.resources {
		out.Items[k] = resourceMarshaler{Resource: v}
	}

	return json.Marshal(out)
}

type resourceMarshaler struct {
	types.Resource
}

func (r resourceMarshaler) MarshalJSON() ([]byte, error) {
	return protojson.Marshal(r.Resource)
}
