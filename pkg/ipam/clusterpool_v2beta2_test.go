package ipam

import (
	"errors"
	"fmt"
	"net"
	"testing"

	"github.com/cilium/ipam/service/ipallocator"
	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cilium/cilium/pkg/ipam/types"
	ciliumv2 "github.com/cilium/cilium/pkg/k8s/apis/cilium.io/v2"
	nodeTypes "github.com/cilium/cilium/pkg/node/types"
	"github.com/cilium/cilium/pkg/trigger"
)

func Test_ClusterPoolManager(t *testing.T) {
	fakeConfig := &testConfiguration{}
	fakeOwner := &ownerMock{}
	events := make(chan string, 1)
	fakeK8sCiliumNodeAPI := &fakeK8sCiliumNodeAPI{
		node: &ciliumv2.CiliumNode{},
		onDeleteEvent: func() {
			events <- "delete"
		},
		onUpsertEvent: func() {
			events <- "upsert"
		},
	}
	c := newClusterPoolManager(fakeConfig, fakeK8sCiliumNodeAPI, fakeOwner, fakeK8sCiliumNodeAPI)
	// set custom preAllocMap to not rely on option.Config in unit tests
	c.preallocMap = preAllocMap{
		"default": 16,
		"mars":    8,
	}
	// For testing, we want every trigger to run the controller once
	k8sUpdater, err := trigger.NewTrigger(trigger.Parameters{
		MinInterval: 0,
		TriggerFunc: func(reasons []string) {
			c.controller.TriggerController(clusterPoolStatusControllerName)
		},
		Name: clusterPoolStatusTriggerName,
	})
	assert.Nil(t, err)
	c.k8sUpdater = k8sUpdater

	currentNode := &ciliumv2.CiliumNode{
		ObjectMeta: metav1.ObjectMeta{Name: nodeTypes.GetName()},
	}
	// provide initial CiliumNode CRD - we expect the agent to request the preAlloc pools
	fakeK8sCiliumNodeAPI.updateNode(currentNode)
	assert.Equal(t, <-events, "upsert")

	// Wait for agent pre-allocation request, then validate it
	assert.Equal(t, <-events, "upsert")
	currentNode = fakeK8sCiliumNodeAPI.currentNode()
	assert.Equal(t, &ciliumv2.CiliumNode{
		ObjectMeta: metav1.ObjectMeta{Name: nodeTypes.GetName()},
		Spec: ciliumv2.NodeSpec{IPAM: types.IPAMSpec{
			Pools: types.IPAMPoolSpec{
				Requested: []types.IPAMPoolRequest{
					{Pool: "mars", Needed: types.IPAMPoolDemand{IPv4Addrs: 8, IPv6Addrs: 8}},
					{Pool: "default", Needed: types.IPAMPoolDemand{IPv4Addrs: 16, IPv6Addrs: 16}},
				},
			},
		}},
		Status: ciliumv2.NodeStatus{
			IPAM: types.IPAMStatus{
				Pools: []types.IPAMPoolStatus{},
			},
		},
	}, currentNode)

	// Assign CIDR to pools (i.e. this simulates the operator logic)
	currentNode.Spec.IPAM.Pools.Allocated = []types.IPAMPoolAllocation{
		{
			Pool: "mars",
			CIDRs: []string{
				"fd00:11::/123",
				"10.0.11.0/27",
			},
		},
		{
			Pool: "default",
			CIDRs: []string{
				"fd00:22::/96",
				"10.0.22.0/24",
			},
		},
	}

	fakeK8sCiliumNodeAPI.updateNode(currentNode)
	assert.Equal(t, <-events, "upsert")
	c.waitForAllPools()

	// test allocation in default pool
	ar, err := c.allocateIP(net.ParseIP("10.0.22.1"), "default-pod-1", "default", IPv4, false)
	assert.Nil(t, err)
	assert.Equal(t, ar.IP, net.ParseIP("10.0.22.1"))

	// cannot allocate the same IP twice
	ar, err = c.allocateIP(net.ParseIP("10.0.22.1"), "default-pod-1", "default", IPv4, false)
	assert.Error(t, err, ipallocator.ErrAllocated)
	assert.Nil(t, ar)

	// allocation from an unknown pool should return an error
	ar, err = c.allocateIP(net.ParseIP("192.168.1.1"), "jupiter-pod-0", "jupiter", IPv4, false)
	assert.ErrorContains(t, err, "unknown pool")
	assert.Nil(t, ar)
	ar, err = c.allocateNext("jupiter-pod-1", "jupiter", IPv6, false)
	assert.ErrorContains(t, err, "unknown pool")
	assert.Nil(t, ar)

	// exhaust mars ipv4 pool (/27 contains 30 IPs)
	allocatedMarsIPs := []net.IP{}
	numMarsIPs := 30
	for i := 0; i < numMarsIPs; i++ {
		// set upstreamSync to true for last allocation, to ensure we only get one upsert event
		ar, err := c.allocateNext(fmt.Sprintf("mars-pod-%d", i), "mars", IPv4, i == numMarsIPs-1)
		assert.Nil(t, err)
		allocatedMarsIPs = append(allocatedMarsIPs, ar.IP)
	}
	_, err = c.allocateNext("mars-pod-overflow", "mars", IPv4, false)
	assert.Error(t, errors.New("all pod CIDR ranges are exhausted"), err)

	// Ensure Requested numbers are bumped
	assert.Equal(t, <-events, "upsert")
	currentNode = fakeK8sCiliumNodeAPI.currentNode()
	assert.Equal(t, []types.IPAMPoolRequest{
		{
			Pool: "mars",
			Needed: types.IPAMPoolDemand{
				IPv4Addrs: 40, // 30 allocated + 8 pre-allocate, rounded up to multiple of 8
				IPv6Addrs: 8,
			}},
		{
			Pool: "default",
			Needed: types.IPAMPoolDemand{
				IPv4Addrs: 32, // 1 allocated + 16 pre-allocate, rounded up to multiple of 16
				IPv6Addrs: 16,
			},
		},
	}, currentNode.Spec.IPAM.Pools.Requested)

	// Assign additional mars IPv4 CIDR
	currentNode.Spec.IPAM.Pools.Allocated = []types.IPAMPoolAllocation{
		{
			Pool: "mars",
			CIDRs: []string{
				"fd00:11::/123",
				"10.0.11.0/27",
				"10.0.12.0/27",
			},
		},
		{
			Pool: "default",
			CIDRs: []string{
				"fd00:22::/96",
				"10.0.22.0/24",
			},
		},
	}
	fakeK8sCiliumNodeAPI.updateNode(currentNode)
	assert.Equal(t, <-events, "upsert")

	// Should now be able to allocate from mars pool again
	ar, err = c.allocateNext("mars-pod-overflow", "mars", IPv4, false)
	assert.Nil(t, err)

	// Deallocate all other IPs from mars pool. This should release the old CIDR
	for i, ip := range allocatedMarsIPs {
		err = c.releaseIP(ip, "mars", IPv4, i == numMarsIPs-1)
		assert.Nil(t, err)
	}
	assert.Equal(t, <-events, "upsert")
	currentNode = fakeK8sCiliumNodeAPI.currentNode()
	assert.Equal(t, []types.IPAMPoolRequest{
		{
			Pool: "mars",
			Needed: types.IPAMPoolDemand{
				IPv4Addrs: 16, // 1 allocated + 8 pre-allocate, rounded up to multiple of 8
				IPv6Addrs: 8,
			}},
		{
			Pool: "default",
			Needed: types.IPAMPoolDemand{
				IPv4Addrs: 32, // 1 allocated + 16 pre-allocate, rounded up to multiple of 16
				IPv6Addrs: 16,
			},
		},
	}, currentNode.Spec.IPAM.Pools.Requested)

	// Initial mars CIDR should have been marked as released now
	assert.Equal(t, []types.IPAMPoolStatus{
		{
			Pool: "mars",
			CIDRs: map[string]types.PodCIDRMapEntry{
				"10.0.11.0/27": {Status: types.PodCIDRStatusReleased},
			},
		},
	}, currentNode.Status.IPAM.Pools)
}
