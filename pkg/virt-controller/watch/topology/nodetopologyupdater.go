package topology

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"time"

	v1 "k8s.io/api/core/v1"
	v12 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"

	"kubevirt.io/client-go/kubecli"

	"kubevirt.io/client-go/log"
)

type NodeTopologyUpdater interface {
	Run(interval time.Duration, stopChan <-chan struct{})
}

type nodeTopologyUpdater struct {
	nodeStore cache.Store
	hinter    Hinter
	client    kubecli.KubevirtClient
}

type stats struct {
	updated int
	skipped int
	error   int
}

func (n *nodeTopologyUpdater) Run(interval time.Duration, stopChan <-chan struct{}) {
	wait.JitterUntil(func() {
		requiredFrequencies := n.requiredFrequencies()
		nodes := FilterNodesFromCache(n.nodeStore.List(),
			HasInvTSCFrequency,
		)
		stats := &stats{}
		for _, node := range nodes {
			nodeCopy, err := calculateNodeLabelChanges(node, requiredFrequencies)
			if err != nil {
				stats.error++
				log.DefaultLogger().Object(node).Reason(err).Error("Could not calculate TSC frequencies for node")
				continue
			}
			if !reflect.DeepEqual(node.Labels, nodeCopy.Labels) {
				if err := patchNode(n.client, node, nodeCopy); err != nil {
					stats.error++
					log.DefaultLogger().Object(node).Reason(err).Error("Could not patch TSC frequencies for node")
					continue
				}
				stats.updated++
			} else {
				stats.skipped++
			}
		}
		log.DefaultLogger().Infof("TSC Freqency node update status: %d updated, %d skipped, %d errors", stats.updated, stats.skipped, stats.error)
	}, interval, 1.2, true, stopChan)
}

func patchNode(client kubecli.KubevirtClient, original *v1.Node, modified *v1.Node) error {
	originalBytes, err := json.Marshal(original)
	if err != nil {
		return fmt.Errorf("could not serialize original object: %v", err)
	}
	modifiedBytes, err := json.Marshal(modified)
	if err != nil {
		return fmt.Errorf("could not serialize modified object: %v", err)
	}
	patch, err := strategicpatch.CreateTwoWayMergePatch(originalBytes, modifiedBytes, v1.Node{})
	if err != nil {
		return fmt.Errorf("could not create merge patch: %v", err)
	}
	if _, err := client.CoreV1().Nodes().Patch(context.Background(), original.Name, types.StrategicMergePatchType, patch, v12.PatchOptions{}); err != nil {
		return fmt.Errorf("could not patch the node: %v", err)
	}
	return nil
}

func calculateNodeLabelChanges(original *v1.Node, requiredFrequencies []int64) (modified *v1.Node, err error) {
	nodeFreq, scalable, err := TSCFrequencyFromNode(original)
	if err != nil {
		log.DefaultLogger().Reason(err).Object(original).Error("Can't determine TSC frequency of the original")
		return nil, err
	}
	freqsOnNode := TSCFrequenciesOnNode(original)
	toAdd, toRemove := CalculateTSCLabelDiff(requiredFrequencies, freqsOnNode, nodeFreq, scalable)
	toAddLabels := ToLabels(toAdd)
	toRemoveLabels := ToLabels(toRemove)

	nodeCopy := original.DeepCopy()
	for _, freq := range toAddLabels {
		nodeCopy.Labels[freq] = "true"
	}
	for _, freq := range toRemoveLabels {
		delete(nodeCopy.Labels, freq)
	}
	return nodeCopy, nil
}

func (n nodeTopologyUpdater) requiredFrequencies() []int64 {
	lowestFrequency, err := n.hinter.LowestTSCFrequencyOnCluster()
	if err != nil {
		log.DefaultLogger().Reason(err).Error("Failed to calculate lowest TSC frequency for nodes")
	}
	return append(n.hinter.TSCFrequenciesInUse(), lowestFrequency)
}

func NewNodeTopologyUpdater(clientset kubecli.KubevirtClient, hinter Hinter, nodeStore cache.Store) NodeTopologyUpdater {
	return &nodeTopologyUpdater{
		client:    clientset,
		hinter:    hinter,
		nodeStore: nodeStore,
	}
}
