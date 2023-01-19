// Copyright 2017 Microsoft. All rights reserved.
// MIT License

package network

import (
	"net"
	"runtime"
	"sync"
	"time"

	cnms "github.com/Azure/azure-container-networking/cnms/cnmspackage"
	"github.com/Azure/azure-container-networking/common"
	"github.com/Azure/azure-container-networking/log"
	"github.com/Azure/azure-container-networking/netio"
	"github.com/Azure/azure-container-networking/netlink"
	"github.com/Azure/azure-container-networking/platform"
	"github.com/Azure/azure-container-networking/store"
	"github.com/pkg/errors"
)

const (
	// Network store key.
	storeKey    = "Network"
	VlanIDKey   = "VlanID"
	AzureCNS    = "azure-cns"
	SNATIPKey   = "NCPrimaryIPKey"
	RoutesKey   = "RoutesKey"
	IPTablesKey = "IPTablesKey"
	genericData = "com.docker.network.generic"
)

var Ipv4DefaultRouteDstPrefix = net.IPNet{
	IP:   net.IPv4zero,
	Mask: net.IPv4Mask(0, 0, 0, 0),
}

type NetworkClient interface {
	CreateBridge() error
	DeleteBridge() error
	AddL2Rules(extIf *externalInterface) error
	DeleteL2Rules(extIf *externalInterface)
	SetBridgeMasterToHostInterface() error
	SetHairpinOnHostInterface(bool) error
}

type EndpointClient interface {
	AddEndpoints(epInfo *EndpointInfo) error
	AddEndpointRules(epInfo *EndpointInfo) error
	DeleteEndpointRules(ep *endpoint)
	MoveEndpointsToContainerNS(epInfo *EndpointInfo, nsID uintptr) error
	SetupContainerInterfaces(epInfo *EndpointInfo) error
	ConfigureContainerInterfacesAndRoutes(epInfo *EndpointInfo) error
	DeleteEndpoints(ep *endpoint) error
}

// NetworkManager manages the set of container networking resources.
type networkManager struct {
	Version            string
	TimeStamp          time.Time
	ExternalInterfaces map[string]*externalInterface
	store              store.KeyValueStore
	netlink            netlink.NetlinkInterface
	netio              netio.NetIOInterface
	plClient           platform.ExecClient
	sync.Mutex
}

// NetworkManager API.
type NetworkManager interface {
	Initialize(config *common.PluginConfig, isRehydrationRequired bool) error
	Uninitialize()

	AddExternalInterface(ifName string, subnet string) error

	CreateNetwork(nwInfo *NetworkInfo) error
	DeleteNetwork(networkID string) error
	GetNetworkInfo(networkID string) (NetworkInfo, error)
	// FindNetworkIDFromNetNs returns the network name that contains an endpoint created for this netNS, errNetworkNotFound if no network is found
	FindNetworkIDFromNetNs(netNs string) (string, error)

	CreateEndpoint(client apipaClient, networkID string, epInfo *EndpointInfo) error
	DeleteEndpoint(networkID string, endpointID string) error
	GetEndpointInfo(networkID string, endpointID string) (*EndpointInfo, error)
	GetAllEndpoints(networkID string) (map[string]*EndpointInfo, error)
	GetEndpointInfoBasedOnPODDetails(networkID string, podName string, podNameSpace string, doExactMatchForPodName bool) (*EndpointInfo, error)
	AttachEndpoint(networkID string, endpointID string, sandboxKey string) (*endpoint, error)
	DetachEndpoint(networkID string, endpointID string) error
	UpdateEndpoint(networkID string, existingEpInfo *EndpointInfo, targetEpInfo *EndpointInfo) error
	GetNumberOfEndpoints(ifName string, networkID string) int
	SetupNetworkUsingState(networkMonitor *cnms.NetworkMonitor) error
}

// Creates a new network manager.
func NewNetworkManager(nl netlink.NetlinkInterface, plc platform.ExecClient, netioCli netio.NetIOInterface) (NetworkManager, error) {
	nm := &networkManager{
		ExternalInterfaces: make(map[string]*externalInterface),
		netlink:            nl,
		plClient:           plc,
		netio:              netioCli,
	}

	return nm, nil
}

// Initialize configures network manager.
func (nm *networkManager) Initialize(config *common.PluginConfig, isRehydrationRequired bool) error {
	nm.Version = config.Version
	nm.store = config.Store

	// Restore persisted state.
	err := nm.restore(isRehydrationRequired)
	return err
}

// Uninitialize cleans up network manager.
func (nm *networkManager) Uninitialize() {
}

// Restore reads network manager state from persistent store.
func (nm *networkManager) restore(isRehydrationRequired bool) error {
	// Skip if a store is not provided.
	if nm.store == nil {
		log.Printf("[net] network store is nil")
		return nil
	}

	rebooted := false
	// After a reboot, all address resources are implicitly released.
	// Ignore the persisted state if it is older than the last reboot time.

	// Acquiring store lock at this stage for optimization purpuses on Windows
	if runtime.GOOS == "windows" {
		// Acquire store lock.
		if err := nm.store.Lock(store.DefaultLockTimeout); err != nil {
			log.Printf("[cni] Failed to lock store: %v.", err)
			return errors.Wrap(err, "error Acquiring store lock")
		}
		// Remove the lock on the key-value store
		defer func() {
			err := nm.store.Unlock()
			if err != nil {
				log.Printf("[cni] Failed to unlock store: %v.", err)
			}
		}()
	}

	// Read any persisted state.
	err := nm.store.Read(storeKey, nm)
	if err != nil {
		if err == store.ErrKeyNotFound {
			log.Printf("[net] network store key not found")
			// Considered successful.
			return nil
		} else if err == store.ErrStoreEmpty {
			log.Printf("[net] network store empty")
			return nil
		} else {
			log.Printf("[net] Failed to restore state, err:%v\n", err)
			return err
		}
	}

	if isRehydrationRequired {
		modTime, err := nm.store.GetModificationTime()
		if err == nil {
			rebootTime, err := platform.GetLastRebootTime()
			log.Printf("[net] reboot time %v store mod time %v", rebootTime, modTime)
			if err == nil && rebootTime.After(modTime) {
				log.Printf("[net] Detected Reboot")
				rebooted = true
				if clearNwConfig, err := platform.ClearNetworkConfiguration(); clearNwConfig {
					if err != nil {
						log.Printf("[net] Failed to clear network configuration, err:%v\n", err)
						return err
					}

					// Delete the networks left behind after reboot
					for _, extIf := range nm.ExternalInterfaces {
						for _, nw := range extIf.Networks {
							log.Printf("[net] Deleting the network %s on reboot\n", nw.Id)
							_ = nm.deleteNetwork(nw.Id)
						}
					}

					// Clear networkManager contents
					nm.TimeStamp = time.Time{}
					for extIfName := range nm.ExternalInterfaces {
						delete(nm.ExternalInterfaces, extIfName)
					}
					return nil
				}
			}
		}
	}
	// Populate pointers.
	for _, extIf := range nm.ExternalInterfaces {
		for _, nw := range extIf.Networks {
			nw.extIf = extIf
		}
	}

	// if rebooted recreate the network that existed before reboot.
	if rebooted {
		log.Printf("[net] Rehydrating network state from persistent store")
		for _, extIf := range nm.ExternalInterfaces {
			for _, nw := range extIf.Networks {
				nwInfo, err := nm.GetNetworkInfo(nw.Id)
				if err != nil {
					log.Printf("[net] Failed to fetch network info for network %v extif %v err %v. This should not happen", nw, extIf, err)
					return err
				}

				extIf.BridgeName = ""

				_, err = nm.newNetworkImpl(&nwInfo, extIf)
				if err != nil {
					log.Printf("[net] Restoring network failed for nwInfo %v extif %v. This should not happen %v", nwInfo, extIf, err)
					return err
				}
			}
		}
	}

	log.Printf("[net] Restored state")
	for _, extIf := range nm.ExternalInterfaces {
		for _, nw := range extIf.Networks {
			log.Printf("Number of endpoints: %d", len(nw.Endpoints))
		}
	}

	return nil
}

// Save writes network manager state to persistent store.
func (nm *networkManager) save() error {
	// Skip if a store is not provided.
	if nm.store == nil {
		return nil
	}

	// Update time stamp.
	nm.TimeStamp = time.Now()

	// Acquiring store lock at this stage for optimization purpuses on Windows
	if runtime.GOOS == "windows" {
		// Acquire store lock.
		if err := nm.store.Lock(store.DefaultLockTimeout); err != nil {
			log.Printf("[cni] Failed to lock store: %v.", err)
			return errors.Wrap(err, "error Acquiring store lock")
		}
		// Remove the lock on the key-value store
		defer func() {
			err := nm.store.Unlock()
			if err != nil {
				log.Printf("[cni] Failed to unlock store: %v.", err)
			}
		}()
	}

	err := nm.store.Write(storeKey, nm)
	if err == nil {
		log.Printf("[net] Save succeeded.\n")
	} else {
		log.Printf("[net] Save failed, err:%v\n", err)
	}
	return err
}

//
// NetworkManager API
//
// Provides atomic stateful wrappers around core networking functionality.
//

// AddExternalInterface adds a host interface to the list of available external interfaces.
func (nm *networkManager) AddExternalInterface(ifName string, subnet string) error {
	nm.Lock()
	defer nm.Unlock()

	err := nm.newExternalInterface(ifName, subnet)
	if err != nil {
		return err
	}

	err = nm.save()
	if err != nil {
		return err
	}

	return nil
}

// CreateNetwork creates a new container network.
func (nm *networkManager) CreateNetwork(nwInfo *NetworkInfo) error {
	nm.Lock()
	defer nm.Unlock()

	_, err := nm.newNetwork(nwInfo)
	if err != nil {
		return err
	}

	err = nm.save()
	if err != nil {
		return err
	}

	return nil
}

// DeleteNetwork deletes an existing container network.
func (nm *networkManager) DeleteNetwork(networkID string) error {
	nm.Lock()
	defer nm.Unlock()

	err := nm.deleteNetwork(networkID)
	if err != nil {
		return err
	}

	err = nm.save()
	if err != nil {
		return err
	}

	return nil
}

// GetNetworkInfo returns information about the given network.
func (nm *networkManager) GetNetworkInfo(networkId string) (NetworkInfo, error) {
	nm.Lock()
	defer nm.Unlock()

	nw, err := nm.getNetwork(networkId)
	if err != nil {
		return NetworkInfo{}, err
	}

	nwInfo := NetworkInfo{
		Id:               networkId,
		Subnets:          nw.Subnets,
		Mode:             nw.Mode,
		EnableSnatOnHost: nw.EnableSnatOnHost,
		DNS:              nw.DNS,
		Options:          make(map[string]interface{}),
	}

	getNetworkInfoImpl(&nwInfo, nw)

	if nw.extIf != nil {
		nwInfo.BridgeName = nw.extIf.BridgeName
	}

	return nwInfo, nil
}

// CreateEndpoint creates a new container endpoint.
func (nm *networkManager) CreateEndpoint(cli apipaClient, networkID string, epInfo *EndpointInfo) error {
	nm.Lock()
	defer nm.Unlock()

	nw, err := nm.getNetwork(networkID)
	if err != nil {
		return err
	}

	if nw.VlanId != 0 {
		if epInfo.Data[VlanIDKey] == nil {
			log.Printf("overriding endpoint vlanid with network vlanid")
			epInfo.Data[VlanIDKey] = nw.VlanId
		}
	}

	_, err = nw.newEndpoint(cli, nm.netlink, nm.plClient, epInfo)
	if err != nil {
		return err
	}

	err = nm.save()
	if err != nil {
		return err
	}

	return nil
}

// DeleteEndpoint deletes an existing container endpoint.
func (nm *networkManager) DeleteEndpoint(networkID, endpointID string) error {
	nm.Lock()
	defer nm.Unlock()

	nw, err := nm.getNetwork(networkID)
	if err != nil {
		return err
	}

	err = nw.deleteEndpoint(nm.netlink, nm.plClient, endpointID)
	if err != nil {
		return err
	}

	err = nm.save()
	if err != nil {
		return err
	}

	return nil
}

// GetEndpointInfo returns information about the given endpoint.
func (nm *networkManager) GetEndpointInfo(networkId string, endpointId string) (*EndpointInfo, error) {
	nm.Lock()
	defer nm.Unlock()

	nw, err := nm.getNetwork(networkId)
	if err != nil {
		return nil, err
	}

	ep, err := nw.getEndpoint(endpointId)
	if err != nil {
		return nil, err
	}

	return ep.getInfo(), nil
}

func (nm *networkManager) GetAllEndpoints(networkId string) (map[string]*EndpointInfo, error) {
	nm.Lock()
	defer nm.Unlock()

	eps := make(map[string]*EndpointInfo)

	// Special case when CNS invokes CNI, but there is no state, but return gracefully
	if len(nm.ExternalInterfaces) == 0 {
		log.Printf("Network manager has no external interfaces, is the state file populated?")
		return eps, store.ErrStoreEmpty
	}

	nw, err := nm.getNetwork(networkId)
	if err != nil {
		return nil, err
	}

	for epid, ep := range nw.Endpoints {
		eps[epid] = ep.getInfo()
	}

	return eps, nil
}

// GetEndpointInfoBasedOnPODDetails returns information about the given endpoint.
// It returns an error if a single pod has multiple endpoints.
func (nm *networkManager) GetEndpointInfoBasedOnPODDetails(networkID string, podName string, podNameSpace string, doExactMatchForPodName bool) (*EndpointInfo, error) {
	nm.Lock()
	defer nm.Unlock()

	nw, err := nm.getNetwork(networkID)
	if err != nil {
		return nil, err
	}

	ep, err := nw.getEndpointByPOD(podName, podNameSpace, doExactMatchForPodName)
	if err != nil {
		return nil, err
	}

	return ep.getInfo(), nil
}

// AttachEndpoint attaches an endpoint to a sandbox.
func (nm *networkManager) AttachEndpoint(networkId string, endpointId string, sandboxKey string) (*endpoint, error) {
	nm.Lock()
	defer nm.Unlock()

	nw, err := nm.getNetwork(networkId)
	if err != nil {
		return nil, err
	}

	ep, err := nw.getEndpoint(endpointId)
	if err != nil {
		return nil, err
	}

	err = ep.attach(sandboxKey)
	if err != nil {
		return nil, err
	}

	err = nm.save()
	if err != nil {
		return nil, err
	}

	return ep, nil
}

// DetachEndpoint detaches an endpoint from its sandbox.
func (nm *networkManager) DetachEndpoint(networkId string, endpointId string) error {
	nm.Lock()
	defer nm.Unlock()

	nw, err := nm.getNetwork(networkId)
	if err != nil {
		return err
	}

	ep, err := nw.getEndpoint(endpointId)
	if err != nil {
		return err
	}

	err = ep.detach()
	if err != nil {
		return err
	}

	err = nm.save()
	if err != nil {
		return err
	}

	return nil
}

// UpdateEndpoint updates an existing container endpoint.
func (nm *networkManager) UpdateEndpoint(networkID string, existingEpInfo *EndpointInfo, targetEpInfo *EndpointInfo) error {
	nm.Lock()
	defer nm.Unlock()

	nw, err := nm.getNetwork(networkID)
	if err != nil {
		return err
	}

	err = nm.updateEndpoint(nw, existingEpInfo, targetEpInfo)
	if err != nil {
		return err
	}

	err = nm.save()
	if err != nil {
		return err
	}

	return nil
}

func (nm *networkManager) GetNumberOfEndpoints(ifName string, networkId string) int {
	if ifName == "" {
		for key := range nm.ExternalInterfaces {
			ifName = key
			break
		}
	}

	if nm.ExternalInterfaces != nil {
		extIf := nm.ExternalInterfaces[ifName]
		if extIf != nil && extIf.Networks != nil {
			nw := extIf.Networks[networkId]
			if nw != nil && nw.Endpoints != nil {
				return len(nw.Endpoints)
			}
		}
	}

	return 0
}

func (nm *networkManager) SetupNetworkUsingState(networkMonitor *cnms.NetworkMonitor) error {
	return nm.monitorNetworkState(networkMonitor)
}
