// SPDX-License-Identifier: Apache-2.0
//
// Copyright (C) 2022 Renesas Electronics Corporation.
// Copyright (C) 2022 EPAM Systems, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package launcher

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"

	"github.com/aosedge/aos_common/aoserrors"
	"github.com/aosedge/aos_common/aostypes"
	"github.com/aosedge/aos_common/api/cloudprotocol"
	"github.com/aosedge/aos_communicationmanager/imagemanager"
	"github.com/aosedge/aos_communicationmanager/unitconfig"
	log "github.com/sirupsen/logrus"
)

/***********************************************************************************************************************
 * Types
 **********************************************************************************************************************/

type nodeDevice struct {
	name           string
	sharedCount    int
	allocatedCount int
}

type runRequestInfo struct {
	Services  []aostypes.ServiceInfo  `json:"services"`
	Layers    []aostypes.LayerInfo    `json:"layers"`
	Instances []aostypes.InstanceInfo `json:"instances"`
}

type nodeHandler struct {
	storage              Storage
	nodeInfo             cloudprotocol.NodeInfo
	availableResources   []string
	availableLabels      []string
	availableDevices     []nodeDevice
	priority             uint32
	receivedRunInstances []cloudprotocol.InstanceStatus
	currentRunRequest    *runRequestInfo
	isLocalNode          bool
	waitStatus           bool
}

/***********************************************************************************************************************
 * Private
 **********************************************************************************************************************/

func newNodeHandler(
	nodeInfo cloudprotocol.NodeInfo, resourceManager ResourceManager, storage Storage, isLocalNode bool,
) (*nodeHandler, error) {
	log.WithFields(log.Fields{"nodeID": nodeInfo.NodeID}).Debug("Init node handler")

	node := &nodeHandler{
		storage:           storage,
		nodeInfo:          nodeInfo,
		currentRunRequest: &runRequestInfo{},
		isLocalNode:       isLocalNode,
		waitStatus:        true,
	}

	if err := node.loadNodeRunRequest(); err != nil && !errors.Is(err, ErrNotExist) {
		log.WithFields(log.Fields{"nodeID": nodeInfo.NodeID}).Errorf("Can't load node run request: %v", err)
	}

	nodeConfig, err := resourceManager.GetNodeConfig(node.nodeInfo.NodeID, node.nodeInfo.NodeType)
	if err != nil && !errors.Is(err, unitconfig.ErrNotFound) {
		return nil, aoserrors.Wrap(err)
	}

	node.initNodeConfig(nodeConfig)

	return node, nil
}

func (node *nodeHandler) initNodeConfig(nodeConfig cloudprotocol.NodeConfig) {
	node.priority = nodeConfig.Priority
	node.availableLabels = nodeConfig.Labels
	node.availableResources = make([]string, len(nodeConfig.Resources))
	node.availableDevices = make([]nodeDevice, len(nodeConfig.Devices))

	for i, resource := range nodeConfig.Resources {
		node.availableResources[i] = resource.Name
	}

	for i, device := range nodeConfig.Devices {
		node.availableDevices[i] = nodeDevice{
			name: device.Name, sharedCount: device.SharedCount, allocatedCount: 0,
		}
	}
}

func (node *nodeHandler) loadNodeRunRequest() error {
	currentRunRequestJSON, err := node.storage.GetNodeState(node.nodeInfo.NodeID)
	if err != nil {
		return aoserrors.Wrap(err)
	}

	if err = json.Unmarshal(currentRunRequestJSON, &node.currentRunRequest); err != nil {
		return aoserrors.Wrap(err)
	}

	return nil
}

func (node *nodeHandler) saveNodeRunRequest() error {
	runRequestJSON, err := json.Marshal(node.currentRunRequest)
	if err != nil {
		return aoserrors.Wrap(err)
	}

	if err := node.storage.SetNodeState(node.nodeInfo.NodeID, runRequestJSON); err != nil {
		return aoserrors.Wrap(err)
	}

	return nil
}

func (node *nodeHandler) allocateDevices(serviceDevices []aostypes.ServiceDevice) error {
serviceDeviceLoop:
	for _, serviceDevice := range serviceDevices {
		for i := range node.availableDevices {
			if node.availableDevices[i].name != serviceDevice.Name {
				continue
			}

			if node.availableDevices[i].sharedCount != 0 {
				if node.availableDevices[i].allocatedCount == node.availableDevices[i].sharedCount {
					return aoserrors.Errorf("can't allocate device: %s", serviceDevice.Name)
				}

				node.availableDevices[i].allocatedCount++

				continue serviceDeviceLoop
			}
		}

		return aoserrors.Errorf("can't allocate device: %s", serviceDevice.Name)
	}

	return nil
}

func (node *nodeHandler) nodeHasDesiredDevices(desiredDevices []aostypes.ServiceDevice) bool {
devicesLoop:
	for _, desiredDevice := range desiredDevices {
		for _, nodeDevice := range node.availableDevices {
			if desiredDevice.Name != nodeDevice.name {
				continue
			}

			if nodeDevice.sharedCount == 0 || nodeDevice.allocatedCount != nodeDevice.sharedCount {
				continue devicesLoop
			}
		}

		return false
	}

	return true
}

func (node *nodeHandler) addRunRequest(
	instance aostypes.InstanceInfo, service imagemanager.ServiceInfo, layers []imagemanager.LayerInfo,
) {
	log.WithFields(instanceIdentLogFields(
		instance.InstanceIdent, log.Fields{"node": node.nodeInfo.NodeID})).Debug("Schedule instance on node")

	node.currentRunRequest.Instances = append(node.currentRunRequest.Instances, instance)

	serviceInfo := service.ServiceInfo

	if !node.isLocalNode {
		serviceInfo.URL = service.RemoteURL
	}

	isNewService := true

	for _, oldService := range node.currentRunRequest.Services {
		if reflect.DeepEqual(oldService, serviceInfo) {
			isNewService = false
			break
		}
	}

	if isNewService {
		log.WithFields(log.Fields{
			"serviceID": serviceInfo.ServiceID, "node": node.nodeInfo.NodeID,
		}).Debug("Schedule service on node")

		node.currentRunRequest.Services = append(node.currentRunRequest.Services, serviceInfo)
	}

layerLoopLabel:
	for _, layer := range layers {
		newLayer := layer.LayerInfo

		if !node.isLocalNode {
			newLayer.URL = layer.RemoteURL
		}

		for _, oldLayer := range node.currentRunRequest.Layers {
			if reflect.DeepEqual(newLayer, oldLayer) {
				continue layerLoopLabel
			}
		}

		log.WithFields(log.Fields{
			"digest": newLayer.Digest, "node": node.nodeInfo.NodeID,
		}).Debug("Schedule layer on node")

		node.currentRunRequest.Layers = append(node.currentRunRequest.Layers, newLayer)
	}
}

func getNodesByStaticResources(allNodes []*nodeHandler,
	serviceInfo imagemanager.ServiceInfo, instanceInfo cloudprotocol.InstanceInfo,
) ([]*nodeHandler, error) {
	nodes := getNodeByRunners(allNodes, serviceInfo.Config.Runners)
	if len(nodes) == 0 {
		return nodes, aoserrors.Errorf("no node with runner: %s", serviceInfo.Config.Runners)
	}

	nodes = getNodesByLabels(nodes, instanceInfo.Labels)
	if len(nodes) == 0 {
		return nodes, aoserrors.Errorf("no node with labels %v", instanceInfo.Labels)
	}

	nodes = getNodesByResources(nodes, serviceInfo.Config.Resources)
	if len(nodes) == 0 {
		return nodes, aoserrors.Errorf("no node with resources %v", serviceInfo.Config.Resources)
	}

	return nodes, nil
}

func getNodesByDevices(availableNodes []*nodeHandler, desiredDevices []aostypes.ServiceDevice) ([]*nodeHandler, error) {
	if len(desiredDevices) == 0 {
		return slices.Clone(availableNodes), nil
	}

	nodes := make([]*nodeHandler, 0)

	for _, node := range availableNodes {
		if !node.nodeHasDesiredDevices(desiredDevices) {
			continue
		}

		nodes = append(nodes, node)
	}

	if len(nodes) == 0 {
		return nodes, aoserrors.New("no available device found")
	}

	return nodes, nil
}

func getNodesByResources(nodes []*nodeHandler, desiredResources []string) (newNodes []*nodeHandler) {
	if len(desiredResources) == 0 {
		return nodes
	}

nodeLoop:
	for _, node := range nodes {
		if len(node.availableResources) == 0 {
			continue
		}

		for _, resource := range desiredResources {
			if !slices.Contains(node.availableResources, resource) {
				continue nodeLoop
			}
		}

		newNodes = append(newNodes, node)
	}

	return newNodes
}

func getNodesByLabels(nodes []*nodeHandler, desiredLabels []string) (newNodes []*nodeHandler) {
	if len(desiredLabels) == 0 {
		return nodes
	}

nodeLoop:
	for _, node := range nodes {
		if len(node.availableLabels) == 0 {
			continue
		}

		for _, label := range desiredLabels {
			if !slices.Contains(node.availableLabels, label) {
				continue nodeLoop
			}
		}

		newNodes = append(newNodes, node)
	}

	return newNodes
}

func getNodeByRunners(allNodes []*nodeHandler, runners []string) (nodes []*nodeHandler) {
	if len(runners) == 0 {
		runners = defaultRunners
	}

	for _, runner := range runners {
		for _, node := range allNodes {
			nodeRunners, err := node.nodeInfo.GetNodeRunners()
			if err != nil {
				log.WithField("nodeID", node.nodeInfo.NodeID).Errorf("Can't get node runners: %v", err)

				continue
			}

			if (len(nodeRunners) == 0 && slices.Contains(defaultRunners, runner)) ||
				slices.Contains(nodeRunners, runner) {
				nodes = append(nodes, node)
			}
		}
	}

	return nodes
}

func getMostPriorityNode(nodes []*nodeHandler) *nodeHandler {
	if len(nodes) == 1 {
		return nodes[0]
	}

	maxNodePriorityIndex := 0

	for i := 1; i < len(nodes); i++ {
		if nodes[maxNodePriorityIndex].priority < nodes[i].priority {
			maxNodePriorityIndex = i
		}
	}

	return nodes[maxNodePriorityIndex]
}