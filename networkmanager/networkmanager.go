// SPDX-License-Identifier: Apache-2.0
//
// Copyright (C) 2023 Renesas Electronics Corporation.
// Copyright (C) 2023 EPAM Systems, Inc.
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

// Package networkmanager provides set of API to configure network

package networkmanager

import (
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"net"
	"path/filepath"
	"strings"
	"sync"

	"github.com/aosedge/aos_common/aoserrors"
	"github.com/aosedge/aos_common/aostypes"
	"github.com/aosedge/aos_communicationmanager/config"

	log "github.com/sirupsen/logrus"
)

/**********************************************************************************************************************
* Consts
**********************************************************************************************************************/

const (
	vlanIDCapacity                = 4096
	allowedConnectionsExpectedLen = 3
	exposePortConfigExpectedLen   = 2
)

/***********************************************************************************************************************
 * Types
 **********************************************************************************************************************/

// Storage provides API to create, remove or access information from DB.
type Storage interface {
	AddNetworkInstanceInfo(info InstanceNetworkInfo) error
	RemoveNetworkInstanceInfo(instance aostypes.InstanceIdent) error
	GetNetworkInstancesInfo() ([]InstanceNetworkInfo, error)
	RemoveNetworkInfo(networkID string, nodeID string) error
	AddNetworkInfo(info NetworkParametersStorage) error
	GetNetworksInfo() ([]NetworkParametersStorage, error)
}

// NodeManager nodes controller.
type NodeManager interface {
	UpdateNetwork(nodeID string, networkParameters []aostypes.NetworkParameters) error
}

type NetworkParametersStorage struct {
	aostypes.NetworkParameters
	NodeID string
}

// NetworkManager networks manager instance.
type NetworkManager struct {
	sync.RWMutex
	instancesData    map[string]map[aostypes.InstanceIdent]InstanceNetworkInfo
	providerNetworks map[string][]NetworkParametersStorage
	ipamSubnet       *ipSubnet
	dns              *dnsServer
	storage          Storage
	nodeManager      NodeManager
}

// FirewallRule represents firewall rule.
type FirewallRule struct {
	Protocol string `json:"protocol"`
	Port     string `json:"port"`
}

// InstanceNetworkInfo represents network info for instance.
type InstanceNetworkInfo struct {
	aostypes.InstanceIdent
	aostypes.NetworkParameters
	Rules []FirewallRule `json:"rules"`
}

// NetworkParameters represents network parameters.
type NetworkParameters struct {
	Hosts            []string
	AllowConnections []string
	ExposePorts      []string
}

/***********************************************************************************************************************
 * Vars
 **********************************************************************************************************************/

// These global variable is used to be able to mocking the functionality of networking in tests.
//
//nolint:gochecknoglobals
var (
	GetIPSubnet func(networkID string) (allocIPNet *net.IPNet, ip net.IP, err error)
	GetVlanID   func(networkID string) (uint64, error)
)

var errRuleNotFound = aoserrors.New("rule not found")

/***********************************************************************************************************************
 * Public
 **********************************************************************************************************************/

// New creates network manager instance.
func New(storage Storage, nodeManager NodeManager, config *config.Config) (*NetworkManager, error) {
	log.Debug("Create network manager")

	ipamSubnet, err := newIPam()
	if err != nil {
		return nil, err
	}

	dns, err := newDNSServer(filepath.Join(config.WorkingDir, "network"), config.DNSIP)
	if err != nil {
		return nil, err
	}

	if GetIPSubnet == nil {
		GetIPSubnet = ipamSubnet.prepareSubnet
	}

	networkManager := &NetworkManager{
		instancesData:    make(map[string]map[aostypes.InstanceIdent]InstanceNetworkInfo),
		providerNetworks: make(map[string][]NetworkParametersStorage),
		ipamSubnet:       ipamSubnet,
		dns:              dns,
		storage:          storage,
		nodeManager:      nodeManager,
	}

	if GetVlanID == nil {
		GetVlanID = networkManager.getVlanID
	}

	networksInfo, err := storage.GetNetworksInfo()
	if err != nil {
		return nil, aoserrors.Wrap(err)
	}

	for _, networkInfo := range networksInfo {
		networkManager.providerNetworks[networkInfo.NetworkID] = append(
			networkManager.providerNetworks[networkInfo.NetworkID], networkInfo)
	}

	networkInstancesInfos, err := storage.GetNetworkInstancesInfo()
	if err != nil {
		return nil, aoserrors.Wrap(err)
	}

	for _, networkInfo := range networkInstancesInfos {
		if len(networkManager.instancesData[networkInfo.NetworkID]) == 0 {
			networkManager.instancesData[networkInfo.NetworkID] = make(
				map[aostypes.InstanceIdent]InstanceNetworkInfo)
		}

		networkInfo.DNSServers = []string{networkManager.dns.IPAddress}
		networkManager.instancesData[networkInfo.NetworkID][networkInfo.InstanceIdent] = networkInfo
	}

	ipamSubnet.removeAllocatedSubnets(networksInfo, networkInstancesInfos)

	return networkManager, nil
}

// RemoveInstanceNetworkConf removes stored instance network parameters.
func (manager *NetworkManager) RemoveInstanceNetworkParameters(instanceIdent aostypes.InstanceIdent) {
	manager.Lock()
	defer manager.Unlock()

	networkParameters, networkID, found := manager.getNetworkParametersToCache(instanceIdent)
	if !found {
		return
	}

	if err := manager.removeInstanceNetworkParameters(
		networkID, instanceIdent, net.IP(networkParameters.IP)); err != nil {
		log.Errorf("Can't remove network info: %v", err)
	}
}

// GetInstances gets instances.
func (manager *NetworkManager) GetInstances() []aostypes.InstanceIdent {
	manager.Lock()
	defer manager.Unlock()

	var instances []aostypes.InstanceIdent

	for _, instancesData := range manager.instancesData {
		for instanceIdent := range instancesData {
			instances = append(instances, instanceIdent)
		}
	}

	return instances
}

// UpdateProviderNetwork updates provider network.
func (manager *NetworkManager) UpdateProviderNetwork(providers []string, nodeID string) error {
	manager.Lock()
	defer manager.Unlock()

	manager.removeProviderNetworks(providers, nodeID)

	networkParameters, err := manager.addProviderNetworks(providers, nodeID)
	if err != nil {
		return err
	}

	return aoserrors.Wrap(manager.nodeManager.UpdateNetwork(nodeID, networkParameters))
}

// Restart restarts DNS server.
func (manager *NetworkManager) RestartDNSServer() error {
	if err := manager.dns.rewriteHostsFile(); err != nil {
		return err
	}

	manager.dns.cleanCacheHosts()

	return manager.dns.restart()
}

// PrepareInstanceNetworkParameters prepares network parameters for instance.
func (manager *NetworkManager) PrepareInstanceNetworkParameters(
	instanceIdent aostypes.InstanceIdent, networkID string, params NetworkParameters,
) (networkParameters aostypes.NetworkParameters, err error) {
	if instanceIdent.ServiceID != "" && instanceIdent.SubjectID != "" {
		params.Hosts = append(
			params.Hosts, fmt.Sprintf(
				"%d.%s.%s", instanceIdent.Instance, instanceIdent.SubjectID, instanceIdent.ServiceID))

		params.Hosts = append(
			params.Hosts, fmt.Sprintf(
				"%d.%s.%s.%s", instanceIdent.Instance, instanceIdent.SubjectID, instanceIdent.ServiceID, networkID))

		if instanceIdent.Instance == 0 {
			params.Hosts = append(params.Hosts, fmt.Sprintf("%s.%s", instanceIdent.SubjectID, instanceIdent.ServiceID))
			params.Hosts = append(
				params.Hosts, fmt.Sprintf(
					"%s.%s.%s", instanceIdent.SubjectID, instanceIdent.ServiceID, networkID))
		}
	}

	networkParameters, currentNetworkID, found := manager.getNetworkParametersToCache(instanceIdent)
	if found && networkID != currentNetworkID {
		if err := manager.removeInstanceNetworkParameters(
			networkID, instanceIdent, net.IP(networkParameters.IP)); err != nil {
			log.Errorf("Can't remove network info: %v", err)
		}

		found = false
	}

	if !found {
		if networkParameters, err = manager.createNetwork(instanceIdent, networkID, params); err != nil {
			return networkParameters, err
		}
	}

	if err := manager.dns.addHosts(params.Hosts, networkParameters.IP); err != nil {
		return networkParameters, err
	}

	if len(params.AllowConnections) > 0 {
		firewallRules, err := manager.prepareFirewallRules(
			networkParameters.Subnet, networkParameters.IP, params.AllowConnections)
		if err != nil {
			return networkParameters, err
		}

		networkParameters.FirewallRules = firewallRules
	}

	return networkParameters, nil
}

/***********************************************************************************************************************
 * Private
 **********************************************************************************************************************/

func (manager *NetworkManager) removeInstanceNetworkParameters(
	networkID string, instanceIdent aostypes.InstanceIdent, ip net.IP,
) error {
	manager.deleteNetworkParametersFromCache(networkID, instanceIdent, ip)

	if err := manager.storage.RemoveNetworkInstanceInfo(instanceIdent); err != nil {
		return aoserrors.Wrap(err)
	}

	return nil
}

func (manager *NetworkManager) createNetwork(
	instanceIdent aostypes.InstanceIdent, networkID string, params NetworkParameters,
) (networkParameters aostypes.NetworkParameters, err error) {
	var (
		ip     net.IP
		subnet *net.IPNet
	)

	defer func() {
		if err != nil {
			manager.deleteNetworkParametersFromCache(networkID, instanceIdent, ip)
		}
	}()

	subnet, ip, err = GetIPSubnet(networkID)
	if err != nil {
		return networkParameters, err
	}

	networkParameters.NetworkID = networkID
	networkParameters.IP = ip.String()
	networkParameters.Subnet = subnet.String()
	networkParameters.DNSServers = []string{manager.dns.IPAddress}

	instanceNetworkInfo := InstanceNetworkInfo{
		InstanceIdent:     instanceIdent,
		NetworkParameters: networkParameters,
	}

	if len(params.ExposePorts) > 0 {
		instanceNetworkInfo.Rules, err = parseExposedPorts(params.ExposePorts)
		if err != nil {
			return networkParameters, err
		}
	}

	if err := manager.storage.AddNetworkInstanceInfo(instanceNetworkInfo); err != nil {
		return networkParameters, aoserrors.Wrap(err)
	}

	manager.addNetworkParametersToCache(instanceNetworkInfo)

	return networkParameters, nil
}

func (manager *NetworkManager) prepareFirewallRules(
	subnet, ip string, allowConnection []string,
) (rules []aostypes.FirewallRule, err error) {
	for _, connection := range allowConnection {
		serviceID, port, protocol, err := parseAllowConnection(connection)
		if err != nil {
			return nil, err
		}

		instanceRule, err := manager.getInstanceRule(serviceID, subnet, port, protocol, ip)
		if err != nil {
			if !errors.Is(err, errRuleNotFound) {
				return nil, err
			}

			continue
		}

		rules = append(rules, instanceRule)
	}

	return rules, nil
}

func (manager *NetworkManager) getInstanceRule(
	serviceID, subnet, port, protocol, ip string,
) (rule aostypes.FirewallRule, err error) {
	for _, instances := range manager.instancesData {
		for _, instanceNetworkInfo := range instances {
			if instanceNetworkInfo.ServiceID != serviceID {
				continue
			}

			same, err := checkIPInSubnet(subnet, instanceNetworkInfo.NetworkParameters.IP)
			if err != nil {
				return rule, err
			}

			if same {
				continue
			}

			if ruleExists(instanceNetworkInfo, port, protocol) {
				return aostypes.FirewallRule{
					DstIP:   instanceNetworkInfo.NetworkParameters.IP,
					SrcIP:   ip,
					Proto:   protocol,
					DstPort: port,
				}, nil
			}
		}
	}

	return rule, errRuleNotFound
}

func checkIPInSubnet(subnet, ip string) (bool, error) {
	_, ipnet, err := net.ParseCIDR(subnet)
	if err != nil {
		return false, aoserrors.Wrap(err)
	}

	parsedIP := net.ParseIP(ip)
	if parsedIP == nil {
		return false, aoserrors.Errorf("invalid IP %s", ip)
	}

	return ipnet.Contains(parsedIP), nil
}

func (manager *NetworkManager) deleteNetworkParametersFromCache(
	networkID string, instanceIdent aostypes.InstanceIdent, ip net.IP,
) {
	delete(manager.instancesData[networkID], instanceIdent)
	delete(manager.dns.hosts, ip.String())

	manager.ipamSubnet.releaseIPToSubnet(networkID, ip)
}

func (manager *NetworkManager) addNetworkParametersToCache(instanceNetworkInfo InstanceNetworkInfo) {
	manager.Lock()
	defer manager.Unlock()

	if _, ok := manager.instancesData[instanceNetworkInfo.NetworkID]; !ok {
		manager.instancesData[instanceNetworkInfo.NetworkID] = make(map[aostypes.InstanceIdent]InstanceNetworkInfo)
	}

	manager.instancesData[instanceNetworkInfo.NetworkID][instanceNetworkInfo.InstanceIdent] = instanceNetworkInfo
}

func (manager *NetworkManager) getNetworkParametersToCache(
	instanceIdent aostypes.InstanceIdent,
) (params aostypes.NetworkParameters, networkID string, found bool) {
	for networkID, instanceData := range manager.instancesData {
		if networkParameter, ok := instanceData[instanceIdent]; ok {
			return networkParameter.NetworkParameters, networkID, true
		}
	}

	return aostypes.NetworkParameters{}, "", false
}

func (manager *NetworkManager) removeProviderNetworks(providers []string, nodeID string) {
	for networkID, networksInfo := range manager.providerNetworks {
		var validNetworks []NetworkParametersStorage

		// If network is not assigned to any node, remove it
		for _, info := range networksInfo {
			if info.NodeID == "" {
				if err := manager.storage.RemoveNetworkInfo(networkID, ""); err != nil {
					log.Errorf("Can't remove network info: %v", err)
				}

				continue
			}

			validNetworks = append(validNetworks, info)
		}

		providerFound := false

		for _, providerID := range providers {
			if networkID == providerID {
				providerFound = true

				break
			}
		}

		if providerFound && len(validNetworks) > 0 {
			manager.providerNetworks[networkID] = validNetworks

			continue
		}

		log.Debugf("Remove provider network %s for node %s", networkID, nodeID)

		var filteredNetworks []NetworkParametersStorage

		for _, info := range validNetworks {
			if info.NodeID == nodeID {
				if err := manager.storage.RemoveNetworkInfo(networkID, nodeID); err != nil {
					log.Errorf("Can't remove network info: %v", err)
				}

				continue
			}

			filteredNetworks = append(filteredNetworks, info)
		}

		manager.providerNetworks[networkID] = filteredNetworks

		if len(filteredNetworks) > 0 {
			continue
		}

		for instanceIdent, netInfo := range manager.instancesData[networkID] {
			if err := manager.removeInstanceNetworkParameters(
				networkID, instanceIdent, net.IP(netInfo.IP)); err != nil {
				log.Errorf("Can't remove network info: %v", err)
			}
		}

		delete(manager.providerNetworks, networkID)
		manager.ipamSubnet.releaseIPNetPool(networkID)
	}
}

func (manager *NetworkManager) setupNetworkParameters(
	providerID string, networkParameter *NetworkParametersStorage,
) error {
	subnet, ip, err := GetIPSubnet(providerID)
	if err != nil {
		return err
	}

	networkParameter.Subnet = subnet.String()
	networkParameter.IP = ip.String()

	if err := manager.storage.AddNetworkInfo(*networkParameter); err != nil {
		return aoserrors.Wrap(err)
	}

	manager.providerNetworks[providerID] = append(manager.providerNetworks[providerID], *networkParameter)

	return nil
}

func (manager *NetworkManager) createProviderNetwork(providerID, nodeID string) (aostypes.NetworkParameters, error) {
	networkParameter := NetworkParametersStorage{
		NetworkParameters: aostypes.NetworkParameters{
			NetworkID: providerID,
		},
		NodeID: nodeID,
	}

	var err error
	if networkParameter.VlanID, err = GetVlanID(providerID); err != nil {
		return aostypes.NetworkParameters{}, err
	}

	if err := manager.setupNetworkParameters(providerID, &networkParameter); err != nil {
		return aostypes.NetworkParameters{}, err
	}

	return networkParameter.NetworkParameters, nil
}

func (manager *NetworkManager) updateProviderNetworkForNode(
	providerID, nodeID string, existingNetwork aostypes.NetworkParameters,
) (aostypes.NetworkParameters, error) {
	networkParameter := NetworkParametersStorage{
		// vlanID should be the same for the same network provider
		NetworkParameters: existingNetwork,
		NodeID:            nodeID,
	}

	if err := manager.setupNetworkParameters(providerID, &networkParameter); err != nil {
		return aostypes.NetworkParameters{}, err
	}

	return networkParameter.NetworkParameters, nil
}

func (manager *NetworkManager) addProviderNetworks(
	providers []string, nodeID string,
) (networkParameters []aostypes.NetworkParameters, err error) {
nextProvider:
	for _, providerID := range providers {
		if networks, ok := manager.providerNetworks[providerID]; ok {
			for _, networkParameter := range networks {
				if networkParameter.NodeID == nodeID {
					networkParameters = append(networkParameters, networkParameter.NetworkParameters)

					continue nextProvider
				}
			}

			netParam, err := manager.updateProviderNetworkForNode(providerID, nodeID, networks[0].NetworkParameters)
			if err != nil {
				return networkParameters, err
			}

			networkParameters = append(networkParameters, netParam)

			continue
		}

		netParam, err := manager.createProviderNetwork(providerID, nodeID)
		if err != nil {
			return networkParameters, err
		}

		networkParameters = append(networkParameters, netParam)
	}

	return networkParameters, nil
}

func (manager *NetworkManager) getVlanID(networkID string) (uint64, error) {
	vlanID, err := rand.Int(rand.Reader, big.NewInt(vlanIDCapacity))
	if err != nil {
		return 0, aoserrors.Wrap(err)
	}

	return vlanID.Uint64() + 1, nil
}

func parseAllowConnection(connection string) (serviceID, port, protocol string, err error) {
	connConf := strings.Split(connection, "/")
	if len(connConf) > allowedConnectionsExpectedLen || len(connConf) < 2 {
		return "", "", "", aoserrors.Errorf("unsupported AllowedConnections format %s", connConf)
	}

	serviceID = connConf[0]
	port = connConf[1]
	protocol = "tcp"

	if len(connConf) == allowedConnectionsExpectedLen {
		protocol = connConf[2]
	}

	return serviceID, port, protocol, nil
}

func ruleExists(info InstanceNetworkInfo, port, protocol string) bool {
	for _, rule := range info.Rules {
		if rule.Port == port && protocol == rule.Protocol {
			return true
		}
	}

	return false
}

func parseExposedPorts(exposePorts []string) ([]FirewallRule, error) {
	rules := make([]FirewallRule, len(exposePorts))

	for i, exposePort := range exposePorts {
		portConfig := strings.Split(exposePort, "/")
		if len(portConfig) > exposePortConfigExpectedLen || len(portConfig) == 0 {
			return nil, aoserrors.Errorf("unsupported ExposedPorts format %s", exposePort)
		}

		protocol := "tcp"
		if len(portConfig) == exposePortConfigExpectedLen {
			protocol = portConfig[1]
		}

		rules[i] = FirewallRule{
			Protocol: protocol,
			Port:     portConfig[0],
		}
	}

	return rules, nil
}
