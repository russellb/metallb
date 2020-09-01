// Copyright 2017 Google Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bytes"
	"crypto/sha256"
	"net"
	"reflect"
	"sort"

	"github.com/go-kit/kit/log"
	"github.com/hashicorp/memberlist"
	"go.universe.tf/metallb/internal/config"
	"go.universe.tf/metallb/internal/layer2"
	v1 "k8s.io/api/core/v1"
)

type layer2Controller struct {
	announcer *layer2.Announce
	myNode    string
	mList     *memberlist.Memberlist
}

// Last known value of the active nodes known via memberlist.
//
// This is used to detect when it changed since last we checked it.
var lastActiveNodes map[string]bool

func (c *layer2Controller) SetConfig(log.Logger, *config.Config) error {
	return nil
}

// usableNodes returns all nodes that have at least one fully ready
// endpoint on them.
func usableNodes(activeNodes map[string]bool, eps *v1.Endpoints) []string {
	usable := map[string]bool{}
	for _, subset := range eps.Subsets {
		for _, ep := range subset.Addresses {
			if ep.NodeName == nil {
				continue
			}
			if activeNodes != nil {
				if _, ok := activeNodes[*ep.NodeName]; !ok {
					continue
				}
			}
			if _, ok := usable[*ep.NodeName]; !ok {
				usable[*ep.NodeName] = true
			}
		}
	}

	var ret []string
	for node, ok := range usable {
		if ok {
			ret = append(ret, node)
		}
	}

	return ret
}

func (c *layer2Controller) getActiveNodes() map[string]bool {
	var activeNodes map[string]bool
	if c.mList != nil {
		activeNodes = map[string]bool{}
		for _, n := range c.mList.Members() {
			activeNodes[n.Name] = true
		}
	}
	return activeNodes
}

func (c *layer2Controller) ShouldAnnounce(l log.Logger, name string, svc *v1.Service, eps *v1.Endpoints) string {
	var activeNodes map[string]bool
	activeNodes = c.getActiveNodes()
	nodes := usableNodes(activeNodes, eps)
	// Sort the slice by the hash of node + service name. This
	// produces an ordering of ready nodes that is unique to this
	// service.
	sort.Slice(nodes, func(i, j int) bool {
		hi := sha256.Sum256([]byte(nodes[i] + "#" + name))
		hj := sha256.Sum256([]byte(nodes[j] + "#" + name))

		return bytes.Compare(hi[:], hj[:]) < 0
	})

	// Are we first in the list? If so, we win and should announce.
	if len(nodes) > 0 && nodes[0] == c.myNode {
		return ""
	}

	// Either not eligible, or lost the election entirely.
	return "notOwner"
}

func (c *layer2Controller) SetBalancer(l log.Logger, name string, lbIP net.IP, pool *config.Pool) error {
	// Determine if the list of active Nodes as determined by memberlist has
	// changed since the last time we checked.  If so, we kick off sending
	// gratuitous ARP / NDP for all IPs we currently own.  This will handle
	// the case where the cluster membership change is recovering from a
	// network partition and we need to establish who the real current owner
	// is of each IP address.  This happens asynchronously.
	var activeNodes map[string]bool
	activeNodes = c.getActiveNodes()
	if !reflect.DeepEqual(activeNodes, lastActiveNodes) {
		c.announcer.AnnounceAll()
		lastActiveNodes = activeNodes
	}

	c.announcer.SetBalancer(name, lbIP)
	return nil
}

func (c *layer2Controller) DeleteBalancer(l log.Logger, name, reason string) error {
	if !c.announcer.AnnounceName(name) {
		return nil
	}
	c.announcer.DeleteBalancer(name)
	return nil
}

func (c *layer2Controller) SetNode(log.Logger, *v1.Node) error {
	return nil
}
