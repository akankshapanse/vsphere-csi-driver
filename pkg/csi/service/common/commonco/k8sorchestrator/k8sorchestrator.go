/*
Copyright 2020 The Kubernetes Authors.

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

package k8sorchestrator

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"k8s.io/client-go/util/retry"

	snapshotterClientSet "github.com/kubernetes-csi/external-snapshotter/client/v8/clientset/versioned"
	cnstypes "github.com/vmware/govmomi/cns/types"
	pbmtypes "github.com/vmware/govmomi/pbm/types"
	"google.golang.org/grpc/codes"
	v1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apiextensionsclientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	clientset "k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientconfig "sigs.k8s.io/controller-runtime/pkg/client/config"
	cnsoperatorv1alpha1 "sigs.k8s.io/vsphere-csi-driver/v3/pkg/apis/cnsoperator"
	wcpcapapis "sigs.k8s.io/vsphere-csi-driver/v3/pkg/apis/wcpcapabilities"
	wcpcapv1alph1 "sigs.k8s.io/vsphere-csi-driver/v3/pkg/apis/wcpcapabilities/v1alpha1"
	cnsvolume "sigs.k8s.io/vsphere-csi-driver/v3/pkg/common/cns-lib/volume"
	cnsconfig "sigs.k8s.io/vsphere-csi-driver/v3/pkg/common/config"
	"sigs.k8s.io/vsphere-csi-driver/v3/pkg/csi/service/common"
	"sigs.k8s.io/vsphere-csi-driver/v3/pkg/csi/service/logger"
	csitypes "sigs.k8s.io/vsphere-csi-driver/v3/pkg/csi/types"
	"sigs.k8s.io/vsphere-csi-driver/v3/pkg/internalapis"
	"sigs.k8s.io/vsphere-csi-driver/v3/pkg/internalapis/featurestates"
	featurestatesv1alpha1 "sigs.k8s.io/vsphere-csi-driver/v3/pkg/internalapis/featurestates/v1alpha1"
	k8s "sigs.k8s.io/vsphere-csi-driver/v3/pkg/kubernetes"
)

const informerCreateRetryInterval = 5 * time.Minute

// operationModeWebHookServer indicates container running as webhook server
const operationModeWebHookServer = "WEBHOOK_SERVER"

var (
	k8sOrchestratorInstance            *K8sOrchestrator
	k8sOrchestratorInstanceInitialized uint32
	// doesSvFssCRExist is set only in Guest cluster flavor if
	// the cnscsisvfeaturestate CR exists in the supervisor namespace
	// of the TKG cluster.
	doesSvFssCRExist         bool
	serviceMode              string
	operationMode            string
	svFssCRMutex             = &sync.RWMutex{}
	k8sOrchestratorInitMutex = &sync.RWMutex{}
	// WcpCapabilitiesMap is the cached map which stores supervisor capabilities name to value map after fetching
	// the data from supervisor-capabilities CR.
	WcpCapabilitiesMap map[string]bool
	// wcpCapabilitiesMapMutex is the mutual exclusion lock used for accessing global map WcpCapabilitiesMap.
	wcpCapabilitiesMapMutex = &sync.RWMutex{}
)

// FSSConfigMapInfo contains details about the FSS configmap(s) present in
// all flavors.
type FSSConfigMapInfo struct {
	featureStatesLock  *sync.RWMutex
	featureStates      map[string]string
	configMapName      string
	configMapNamespace string
}

// Map of volume handles to the pvc it is bound to.
// Key is the volume handle ID and value is the namespaced name of the pvc.
// The methods to add, remove and get entries from the map in a threadsafe
// manner are defined.
type volumeIDToPvcMap struct {
	*sync.RWMutex
	items map[string]string
}

// Map of pvc to volumeID.
// Key is the namespaced pvc name and value volumeID.
// The methods to add, remove and get entries from the map in a threadsafe
// manner are defined.
type pvcToVolumeIDMap struct {
	*sync.RWMutex
	items map[string]string
}

// Adds an entry to volumeIDToPvcMap in a thread safe manner.
func (m *volumeIDToPvcMap) add(volumeHandle, pvcName string) {
	m.Lock()
	defer m.Unlock()
	m.items[volumeHandle] = pvcName
}

// Removes a volume handle from volumeIDToPvcMap in a thread safe manner.
func (m *volumeIDToPvcMap) remove(volumeHandle string) {
	m.Lock()
	defer m.Unlock()
	delete(m.items, volumeHandle)
}

// Returns the namespaced pvc name corresponding to volumeHandle.
func (m *volumeIDToPvcMap) get(volumeHandle string) (string, bool) {
	m.RLock()
	defer m.RUnlock()
	pvcname, found := m.items[volumeHandle]
	return pvcname, found
}

// Adds an entry to pvcToVolumeIDMap in a thread safe manner.
func (m *pvcToVolumeIDMap) add(pvcName, volumeHandle string) {
	m.Lock()
	defer m.Unlock()
	m.items[pvcName] = volumeHandle
}

// Removes a pvcName from pvcToVolumeIDMap in a thread safe manner.
func (m *pvcToVolumeIDMap) remove(pvcName string) {
	m.Lock()
	defer m.Unlock()
	delete(m.items, pvcName)
}

// Returns the volumeID corresponding to the pvc name.
func (m *pvcToVolumeIDMap) get(pvcName string) (string, bool) {
	m.RLock()
	defer m.RUnlock()
	volumeID, found := m.items[pvcName]
	return volumeID, found
}

// Map of the volumeName which refers to the PVName, to the list of node names in the cluster.
// Key is the volume name and value is the list of published nodes for the volume
// The methods to add, remove and get entries from the map in a threadsafe
// manner are defined.
type volumeNameToNodesMap struct {
	*sync.RWMutex
	items map[string][]string
}

// Adds an entry to volumeNameToNodesMap in a thread safe manner.
func (m *volumeNameToNodesMap) add(volumeName string, nodes []string) {
	m.Lock()
	defer m.Unlock()
	m.items[volumeName] = nodes
}

// Removes a volumeName from the volumeNameToNodesMap in a thread safe manner.
func (m *volumeNameToNodesMap) remove(volumeName string) {
	m.Lock()
	defer m.Unlock()
	delete(m.items, volumeName)
}

// Returns the list of published nodes for the given pvName in a thread safe manner.
func (m *volumeNameToNodesMap) get(volumeName string) []string {
	m.RLock()
	defer m.RUnlock()
	return m.items[volumeName]
}

// Map of nodeID to node names in the cluster. Key is the nodeID
// and value is the corresponding node name. The methods to add
// and remove entries from the map in a threadsafe manner are defined.
type nodeIDToNameMap struct {
	*sync.RWMutex
	items map[string]string
}

// Adds an entry to nodeIDToNameMap in a thread safe manner.
func (m *nodeIDToNameMap) add(nodeID, nodeName string) {
	m.Lock()
	defer m.Unlock()
	m.items[nodeID] = nodeName
}

// Removes an entry from nodeIDToNameMap in a thread safe manner.
func (m *nodeIDToNameMap) remove(nodeID string) {
	m.Lock()
	defer m.Unlock()
	delete(m.items, nodeID)
}

// Map of volume ID to volume name.
// Key is the volume ID and value is the volume name.
// The methods to add, remove and get entries from the map in a threadsafe
// manner are defined.
type volumeIDToNameMap struct {
	*sync.RWMutex
	items map[string]string
}

// Adds an entry to volumeNameToIDMap in a thread safe manner.
func (m *volumeIDToNameMap) add(volumeID, volumeName string) {
	m.Lock()
	defer m.Unlock()
	m.items[volumeID] = volumeName
}

// Removes a volume ID from volumeNameToIDMap in a thread safe manner.
func (m *volumeIDToNameMap) remove(volumeID string) {
	m.Lock()
	defer m.Unlock()
	delete(m.items, volumeID)
}

// Returns the volume ID corresponding to volumeName.
func (m *volumeIDToNameMap) get(volumeID string) (string, bool) {
	m.RLock()
	defer m.RUnlock()
	volumeName, found := m.items[volumeID]
	return volumeName, found
}

// K8sOrchestrator defines set of properties specific to K8s.
type K8sOrchestrator struct {
	supervisorFSS        FSSConfigMapInfo
	internalFSS          FSSConfigMapInfo
	releasedVanillaFSS   map[string]struct{}
	informerManager      *k8s.InformerManager
	clusterFlavor        cnstypes.CnsClusterFlavor
	volumeIDToPvcMap     *volumeIDToPvcMap
	pvcToVolumeIDMap     *pvcToVolumeIDMap
	nodeIDToNameMap      *nodeIDToNameMap
	volumeNameToNodesMap *volumeNameToNodesMap // used when ListVolume FSS is enabled
	volumeIDToNameMap    *volumeIDToNameMap    // used when ListVolume FSS is enabled
	k8sClient            clientset.Interface
	snapshotterClient    snapshotterClientSet.Interface
}

// K8sGuestInitParams lists the set of parameters required to run the init for
// K8sOrchestrator in Guest cluster.
type K8sGuestInitParams struct {
	InternalFeatureStatesConfigInfo   cnsconfig.FeatureStatesConfigInfo
	SupervisorFeatureStatesConfigInfo cnsconfig.FeatureStatesConfigInfo
	ServiceMode                       string
	OperationMode                     string
}

// K8sSupervisorInitParams lists the set of parameters required to run the init
// for K8sOrchestrator in Supervisor cluster.
type K8sSupervisorInitParams struct {
	SupervisorFeatureStatesConfigInfo cnsconfig.FeatureStatesConfigInfo
	ServiceMode                       string
	OperationMode                     string
}

// K8sVanillaInitParams lists the set of parameters required to run the init for
// K8sOrchestrator in Vanilla cluster.
type K8sVanillaInitParams struct {
	InternalFeatureStatesConfigInfo cnsconfig.FeatureStatesConfigInfo
	ServiceMode                     string
	OperationMode                   string
}

// Newk8sOrchestrator instantiates K8sOrchestrator object and returns this
// object. NOTE: As Newk8sOrchestrator is created in the init of the driver and
// syncer components, raise an error only if it is of utmost importance.
func Newk8sOrchestrator(ctx context.Context, controllerClusterFlavor cnstypes.CnsClusterFlavor,
	params interface{}) (*K8sOrchestrator, error) {
	var (
		coInstanceErr     error
		k8sClient         clientset.Interface
		snapshotterClient snapshotterClientSet.Interface
	)
	if atomic.LoadUint32(&k8sOrchestratorInstanceInitialized) == 0 {
		k8sOrchestratorInitMutex.Lock()
		defer k8sOrchestratorInitMutex.Unlock()
		if k8sOrchestratorInstanceInitialized == 0 {
			log := logger.GetLogger(ctx)
			log.Info("Initializing k8sOrchestratorInstance")

			// Create a K8s client
			k8sClient, coInstanceErr = k8s.NewClient(ctx)
			if coInstanceErr != nil {
				log.Errorf("Creating Kubernetes client failed. Err: %v", coInstanceErr)
				return nil, coInstanceErr
			}

			// Create a snapshotter client
			snapshotterClient, coInstanceErr = k8s.NewSnapshotterClient(ctx)
			if coInstanceErr != nil {
				log.Errorf("Creating Snapshotter client failed. Err: %v", coInstanceErr)
				return nil, coInstanceErr
			}

			k8sOrchestratorInstance = &K8sOrchestrator{}
			k8sOrchestratorInstance.clusterFlavor = controllerClusterFlavor
			k8sOrchestratorInstance.k8sClient = k8sClient
			k8sOrchestratorInstance.snapshotterClient = snapshotterClient
			k8sOrchestratorInstance.informerManager = k8s.NewInformer(ctx, k8sClient, true)
			coInstanceErr = initFSS(ctx, k8sClient, controllerClusterFlavor, params)
			if coInstanceErr != nil {
				log.Errorf("Failed to initialize the orchestrator. Error: %v", coInstanceErr)
				return nil, coInstanceErr
			}

			if controllerClusterFlavor == cnstypes.CnsClusterFlavorWorkload {
				svInitParams, ok := params.(K8sSupervisorInitParams)
				if !ok {
					return nil, fmt.Errorf("expected orchestrator params of type K8sSupervisorInitParams, got %T instead", params)
				}
				operationMode = svInitParams.OperationMode
			} else if controllerClusterFlavor == cnstypes.CnsClusterFlavorVanilla {
				vanillaInitParams, ok := params.(K8sVanillaInitParams)
				if !ok {
					return nil, fmt.Errorf("expected orchestrator params of type K8sVanillaInitParams, got %T instead", params)
				}
				operationMode = vanillaInitParams.OperationMode
				k8sOrchestratorInstance.releasedVanillaFSS = getReleasedVanillaFSS()
			} else if controllerClusterFlavor == cnstypes.CnsClusterFlavorGuest {
				guestInitParams, ok := params.(K8sGuestInitParams)
				if !ok {
					return nil, fmt.Errorf("expected orchestrator params of type K8sGuestInitParams, got %T instead", params)
				}
				operationMode = guestInitParams.OperationMode
			} else {
				return nil, fmt.Errorf("wrong orchestrator params type")
			}

			if ((controllerClusterFlavor == cnstypes.CnsClusterFlavorWorkload &&
				k8sOrchestratorInstance.IsFSSEnabled(ctx, common.FakeAttach)) ||
				(controllerClusterFlavor == cnstypes.CnsClusterFlavorVanilla &&
					k8sOrchestratorInstance.IsFSSEnabled(ctx, common.ListVolumes))) &&
				(operationMode != operationModeWebHookServer) {
				err := initVolumeHandleToPvcMap(ctx, controllerClusterFlavor)
				if err != nil {
					return nil, fmt.Errorf("failed to create volume handle to PVC map. Error: %v", err)
				}
			}

			if (controllerClusterFlavor == cnstypes.CnsClusterFlavorWorkload) &&
				(operationMode != operationModeWebHookServer) {
				// Initialize the map for volumeName to nodes, as it is needed for WCP detach volume handling
				err := initVolumeNameToNodesMap(ctx, controllerClusterFlavor)
				if err != nil {
					return nil, fmt.Errorf("failed to create PV name to node names map. Error: %v", err)
				}
				err = initNodeIDToNameMap(ctx)
				if err != nil {
					return nil, fmt.Errorf("failed to create node ID to name map. Error: %v", err)
				}
			} else if operationMode != operationModeWebHookServer {
				// Initialize the map for volumeName to nodes, for non-WCP flavors and when ListVolume FSS is on
				if k8sOrchestratorInstance.IsFSSEnabled(ctx, common.ListVolumes) {
					err := initVolumeNameToNodesMap(ctx, controllerClusterFlavor)
					if err != nil {
						return nil, fmt.Errorf("failed to create PV name to node names map. Error: %v", err)
					}
				}
			}

			k8sOrchestratorInstance.informerManager.Listen()
			atomic.StoreUint32(&k8sOrchestratorInstanceInitialized, 1)
			log.Info("k8sOrchestratorInstance initialized")
		}
	}
	return k8sOrchestratorInstance, nil
}

func getReleasedVanillaFSS() map[string]struct{} {
	return map[string]struct{}{
		common.CSIMigration:                   {},
		common.OnlineVolumeExtend:             {},
		common.BlockVolumeSnapshot:            {},
		common.CSIWindowsSupport:              {},
		common.ListVolumes:                    {},
		common.CnsMgrSuspendCreateVolume:      {},
		common.TopologyPreferentialDatastores: {},
		common.MultiVCenterCSITopology:        {},
		common.CSIInternalGeneratedClusterID:  {},
		common.TopologyAwareFileVolume:        {},
	}
}

// initFSS performs all the operations required to initialize the Feature
// states map and keep a watch on it. NOTE: As initFSS is called during the
// init of the driver and syncer components, raise an error only if the
// containers need to crash.
func initFSS(ctx context.Context, k8sClient clientset.Interface,
	controllerClusterFlavor cnstypes.CnsClusterFlavor, params interface{}) error {
	log := logger.GetLogger(ctx)
	var (
		fssConfigMap               *v1.ConfigMap
		err                        error
		configMapNamespaceToListen string
	)
	// Store configmap info in global variables to access later.
	if controllerClusterFlavor == cnstypes.CnsClusterFlavorWorkload {
		k8sOrchestratorInstance.supervisorFSS.featureStatesLock = &sync.RWMutex{}
		k8sOrchestratorInstance.supervisorFSS.featureStates = make(map[string]string)
		// Validate init params
		svInitParams, ok := params.(K8sSupervisorInitParams)
		if !ok {
			return fmt.Errorf("expected orchestrator params of type K8sSupervisorInitParams, got %T instead", params)
		}
		k8sOrchestratorInstance.supervisorFSS.configMapName = svInitParams.SupervisorFeatureStatesConfigInfo.Name
		k8sOrchestratorInstance.supervisorFSS.configMapNamespace = svInitParams.SupervisorFeatureStatesConfigInfo.Namespace
		configMapNamespaceToListen = k8sOrchestratorInstance.supervisorFSS.configMapNamespace
		serviceMode = svInitParams.ServiceMode
	}
	if controllerClusterFlavor == cnstypes.CnsClusterFlavorVanilla {
		k8sOrchestratorInstance.internalFSS.featureStatesLock = &sync.RWMutex{}
		k8sOrchestratorInstance.internalFSS.featureStates = make(map[string]string)
		// Validate init params.
		vanillaInitParams, ok := params.(K8sVanillaInitParams)
		if !ok {
			return fmt.Errorf("expected orchestrator params of type K8sVanillaInitParams, got %T instead", params)
		}
		k8sOrchestratorInstance.internalFSS.configMapName = vanillaInitParams.InternalFeatureStatesConfigInfo.Name
		k8sOrchestratorInstance.internalFSS.configMapNamespace = vanillaInitParams.InternalFeatureStatesConfigInfo.Namespace
		configMapNamespaceToListen = k8sOrchestratorInstance.internalFSS.configMapNamespace
		serviceMode = vanillaInitParams.ServiceMode
	}
	if controllerClusterFlavor == cnstypes.CnsClusterFlavorGuest {
		k8sOrchestratorInstance.supervisorFSS.featureStatesLock = &sync.RWMutex{}
		k8sOrchestratorInstance.supervisorFSS.featureStates = make(map[string]string)
		k8sOrchestratorInstance.internalFSS.featureStatesLock = &sync.RWMutex{}
		k8sOrchestratorInstance.internalFSS.featureStates = make(map[string]string)
		// Validate init params.
		guestInitParams, ok := params.(K8sGuestInitParams)
		if !ok {
			return fmt.Errorf("expected orchestrator params of type K8sGuestInitParams, got %T instead", params)
		}
		k8sOrchestratorInstance.internalFSS.configMapName = guestInitParams.InternalFeatureStatesConfigInfo.Name
		k8sOrchestratorInstance.internalFSS.configMapNamespace = guestInitParams.InternalFeatureStatesConfigInfo.Namespace
		k8sOrchestratorInstance.supervisorFSS.configMapName = guestInitParams.SupervisorFeatureStatesConfigInfo.Name
		k8sOrchestratorInstance.supervisorFSS.configMapNamespace = guestInitParams.SupervisorFeatureStatesConfigInfo.Namespace
		// As of now, TKGS is having both supervisor FSS and internal FSS in the
		// same namespace. If the configmap's namespaces change in future, we may
		// need listeners on different namespaces. Until then, we will initialize
		// configMapNamespaceToListen to internalFSS.configMapNamespace.
		configMapNamespaceToListen = k8sOrchestratorInstance.internalFSS.configMapNamespace
		serviceMode = guestInitParams.ServiceMode
	}

	// Initialize internal FSS map values.
	if controllerClusterFlavor == cnstypes.CnsClusterFlavorGuest ||
		controllerClusterFlavor == cnstypes.CnsClusterFlavorVanilla {
		if k8sOrchestratorInstance.internalFSS.configMapName != "" &&
			k8sOrchestratorInstance.internalFSS.configMapNamespace != "" {
			// Retrieve configmap.
			fssConfigMap, err = k8sClient.CoreV1().ConfigMaps(k8sOrchestratorInstance.internalFSS.configMapNamespace).Get(
				ctx, k8sOrchestratorInstance.internalFSS.configMapName, metav1.GetOptions{})
			if err != nil {
				// return error as we cannot init containers without this info.
				log.Errorf("failed to fetch configmap %s from namespace %s. Error: %v",
					k8sOrchestratorInstance.internalFSS.configMapName,
					k8sOrchestratorInstance.internalFSS.configMapNamespace, err)
				return err
			}
			// Update values.
			k8sOrchestratorInstance.internalFSS.featureStatesLock.Lock()
			k8sOrchestratorInstance.internalFSS.featureStates = fssConfigMap.Data
			log.Infof("New internal feature states values stored successfully: %v",
				k8sOrchestratorInstance.internalFSS.featureStates)
			k8sOrchestratorInstance.internalFSS.featureStatesLock.Unlock()
		}
	}

	if controllerClusterFlavor == cnstypes.CnsClusterFlavorGuest && serviceMode != "node" {
		var isFSSCREnabled bool
		// Check if csi-sv-feature-states-replication FSS exists and is enabled.
		k8sOrchestratorInstance.internalFSS.featureStatesLock.RLock()
		if val, ok := k8sOrchestratorInstance.internalFSS.featureStates[common.CSISVFeatureStateReplication]; ok {
			k8sOrchestratorInstance.internalFSS.featureStatesLock.RUnlock()
			isFSSCREnabled, err = strconv.ParseBool(val)
			if err != nil {
				log.Errorf("unable to convert %v to bool. csi-sv-feature-states-replication FSS disabled. Error: %v",
					val, err)
				return err
			}
		} else {
			k8sOrchestratorInstance.internalFSS.featureStatesLock.RUnlock()
			return logger.LogNewError(log, "csi-sv-feature-states-replication FSS not present")
		}

		// Initialize supervisor FSS map values in GC using the
		// cnscsisvfeaturestate CR if csi-sv-feature-states-replication FSS
		// is enabled.
		if isFSSCREnabled {
			svNamespace, err := cnsconfig.GetSupervisorNamespace(ctx)
			if err != nil {
				log.Errorf("failed to retrieve supervisor cluster namespace from config. Error: %+v", err)
				return err
			}
			cfg, err := cnsconfig.GetConfig(ctx)
			if err != nil {
				log.Errorf("failed to read config. Error: %+v", err)
				return err
			}
			// Get rest client config for supervisor.
			restClientConfig := k8s.GetRestClientConfigForSupervisor(ctx, cfg.GC.Endpoint, cfg.GC.Port)

			// Attempt to fetch the cnscsisvfeaturestate CR from the supervisor
			// namespace of the TKG cluster.
			svFssCR, err := getSVFssCR(ctx, restClientConfig)
			if err != nil {
				// If the cnscsisvfeaturestate CR is not yet registered in the
				// supervisor cluster, we receive NoKindMatchError. In such cases
				// log an info message and fallback to GCM replicated configmap
				// approach.
				_, ok := err.(*apiMeta.NoKindMatchError)
				if ok {
					log.Infof("%s CR not found in supervisor namespace. Defaulting to the %q FSS configmap "+
						"in %q namespace. Error: %+v",
						featurestates.CRDSingular, k8sOrchestratorInstance.supervisorFSS.configMapName,
						k8sOrchestratorInstance.supervisorFSS.configMapNamespace, err)
				} else {
					log.Errorf("failed to get %s CR from supervisor namespace %q. Error: %+v",
						featurestates.CRDSingular, svNamespace, err)
					return err
				}
			} else {
				setSvFssCRAvailability(true)
				// Store supervisor FSS values in cache.
				k8sOrchestratorInstance.supervisorFSS.featureStatesLock.Lock()
				for _, svFSS := range svFssCR.Spec.FeatureStates {
					k8sOrchestratorInstance.supervisorFSS.featureStates[svFSS.Name] = strconv.FormatBool(svFSS.Enabled)
				}
				log.Infof("New supervisor feature states values stored successfully from %s CR object: %v",
					featurestates.SVFeatureStateCRName, k8sOrchestratorInstance.supervisorFSS.featureStates)
				k8sOrchestratorInstance.supervisorFSS.featureStatesLock.Unlock()
			}

			// Create an informer to watch on the cnscsisvfeaturestate CR.
			go func() {
				// Ideally if a resource is not yet registered on a cluster and we
				// try to create an informer to watch it, the informer creation will
				// not fail. But, the informer starts emitting error messages like
				// `Failed to list X: the server could not find the requested resource`.
				// To avoid this, we attempt to fetch the cnscsisvfeaturestate CR
				// first and retry if we receive an error. This is required in cases
				// where TKG cluster is on a newer build and supervisor is at an
				// older version.
				ticker := time.NewTicker(informerCreateRetryInterval)
				var dynInformer informers.GenericInformer
				for range ticker.C {
					// Check if cnscsisvfeaturestate CR exists, if not keep retrying.
					_, err = getSVFssCR(ctx, restClientConfig)
					if err != nil {
						continue
					}
					// Create a dynamic informer for the cnscsisvfeaturestate CR.
					dynInformer, err = k8s.GetDynamicInformer(ctx, featurestates.CRDGroupName,
						internalapis.Version, featurestates.CRDPlural, svNamespace, restClientConfig, false)
					if err != nil {
						log.Errorf("failed to create dynamic informer for %s CR. Error: %+v", featurestates.CRDSingular, err)
						continue
					}
					break
				}
				// Set up namespaced listener for cnscsisvfeaturestate CR.
				_, err = dynInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
					// Add.
					AddFunc: func(obj interface{}) {
						fssCRAdded(obj)
					},
					// Update.
					UpdateFunc: func(oldObj interface{}, newObj interface{}) {
						fssCRUpdated(oldObj, newObj)
					},
					// Delete.
					DeleteFunc: func(obj interface{}) {
						fssCRDeleted(obj)
					},
				})
				if err != nil {
					log.Errorf("failed to add event handler for informer on %q CR. Error: %v",
						featurestates.CRDPlural, err)
					os.Exit(1)
				}
				stopCh := make(chan struct{})
				log.Infof("Informer to watch on %s CR starting..", featurestates.CRDSingular)
				dynInformer.Informer().Run(stopCh)
			}()
		}
	}
	// Initialize supervisor FSS map values using configmap in Supervisor
	// cluster flavor or in guest cluster flavor only if cnscsisvfeaturestate
	// CR is not registered yet.
	if controllerClusterFlavor == cnstypes.CnsClusterFlavorWorkload ||
		(controllerClusterFlavor == cnstypes.CnsClusterFlavorGuest &&
			!getSvFssCRAvailability()) {
		if k8sOrchestratorInstance.supervisorFSS.configMapName != "" &&
			k8sOrchestratorInstance.supervisorFSS.configMapNamespace != "" {
			// Retrieve configmap.
			fssConfigMap, err = k8sClient.CoreV1().ConfigMaps(k8sOrchestratorInstance.supervisorFSS.configMapNamespace).Get(
				ctx, k8sOrchestratorInstance.supervisorFSS.configMapName, metav1.GetOptions{})
			if err != nil {
				log.Errorf("failed to fetch configmap %s from namespace %s. Error: %v",
					k8sOrchestratorInstance.supervisorFSS.configMapName,
					k8sOrchestratorInstance.supervisorFSS.configMapNamespace, err)
				return err
			}
			// Update values.
			k8sOrchestratorInstance.supervisorFSS.featureStatesLock.Lock()
			k8sOrchestratorInstance.supervisorFSS.featureStates = fssConfigMap.Data
			log.Infof("New supervisor feature states values stored successfully: %v",
				k8sOrchestratorInstance.supervisorFSS.featureStates)
			k8sOrchestratorInstance.supervisorFSS.featureStatesLock.Unlock()
		}
	}
	// Set up kubernetes configmap listener for CSI namespace.
	err = k8sOrchestratorInstance.informerManager.AddConfigMapListener(
		ctx,
		k8sClient,
		configMapNamespaceToListen,
		// Add.
		func(obj interface{}) {
			configMapAdded(obj)
		},
		// Update.
		func(oldObj interface{}, newObj interface{}) {
			configMapUpdated(oldObj, newObj)
		},
		// Delete.
		func(obj interface{}) {
			configMapDeleted(obj)
		})
	if err != nil {
		return logger.LogNewErrorf(log, "failed to listen on configmaps in namespace %q. Error: %v",
			configMapNamespaceToListen, err)
	}
	return nil
}

func setSvFssCRAvailability(exists bool) {
	svFssCRMutex.Lock()
	defer svFssCRMutex.Unlock()
	doesSvFssCRExist = exists
}

func getSvFssCRAvailability() bool {
	svFssCRMutex.RLock()
	defer svFssCRMutex.RUnlock()
	return doesSvFssCRExist
}

// getSVFssCR retrieves the cnscsisvfeaturestate CR from the supervisor
// namespace in the TKG cluster using the supervisor client.
// It takes the REST config to the cluster and creates a client using the config
// to returns the svFssCR object.
func getSVFssCR(ctx context.Context, restClientConfig *restclient.Config) (
	*featurestatesv1alpha1.CnsCsiSvFeatureStates, error) {
	log := logger.GetLogger(ctx)

	// Get CNS operator client.
	cnsOperatorClient, err := k8s.NewClientForGroup(ctx, restClientConfig, cnsoperatorv1alpha1.GroupName)
	if err != nil {
		log.Errorf("failed to create CnsOperator client. Err: %+v", err)
		return nil, err
	}
	svNamespace, err := cnsconfig.GetSupervisorNamespace(ctx)
	if err != nil {
		log.Errorf("failed to retrieve supervisor cluster namespace from config. Error: %+v", err)
		return nil, err
	}
	// Fetch cnscsisvfeaturestate CR.
	svFssCR := &featurestatesv1alpha1.CnsCsiSvFeatureStates{}
	err = cnsOperatorClient.Get(ctx, client.ObjectKey{Name: featurestates.SVFeatureStateCRName,
		Namespace: svNamespace}, svFssCR)
	if err != nil {
		log.Debugf("failed to get %s CR from supervisor namespace %q. Error: %+v",
			featurestates.CRDSingular, svNamespace, err)
		return nil, err
	}

	return svFssCR, nil
}

// configMapAdded adds feature state switch values from configmap that has been
// created on K8s cluster.
func configMapAdded(obj interface{}) {
	_, log := logger.GetNewContextWithLogger()
	fssConfigMap, ok := obj.(*v1.ConfigMap)
	if fssConfigMap == nil || !ok {
		log.Warnf("configMapAdded: unrecognized object %+v", obj)
		return
	}

	if fssConfigMap.Name == k8sOrchestratorInstance.supervisorFSS.configMapName &&
		fssConfigMap.Namespace == k8sOrchestratorInstance.supervisorFSS.configMapNamespace {
		if serviceMode == "node" {
			log.Debug("configMapAdded: Ignoring supervisor FSS configmap add event in the nodes")
			return
		}
		if getSvFssCRAvailability() {
			log.Debugf("configMapAdded: Ignoring supervisor FSS configmap add event as %q CR is present",
				featurestates.CRDSingular)
			return
		}
		// Update supervisor FSS.
		k8sOrchestratorInstance.supervisorFSS.featureStatesLock.Lock()
		k8sOrchestratorInstance.supervisorFSS.featureStates = fssConfigMap.Data
		log.Infof("configMapAdded: Supervisor feature state values from %q stored successfully: %v",
			fssConfigMap.Name, k8sOrchestratorInstance.supervisorFSS.featureStates)
		k8sOrchestratorInstance.supervisorFSS.featureStatesLock.Unlock()
	} else if fssConfigMap.Name == k8sOrchestratorInstance.internalFSS.configMapName &&
		fssConfigMap.Namespace == k8sOrchestratorInstance.internalFSS.configMapNamespace {
		// Update internal FSS.
		k8sOrchestratorInstance.internalFSS.featureStatesLock.Lock()
		k8sOrchestratorInstance.internalFSS.featureStates = fssConfigMap.Data
		log.Infof("configMapAdded: Internal feature state values from %q stored successfully: %v",
			fssConfigMap.Name, k8sOrchestratorInstance.internalFSS.featureStates)
		k8sOrchestratorInstance.internalFSS.featureStatesLock.Unlock()
	}
}

// configMapUpdated updates feature state switch values from configmap that
// has been created on K8s cluster.
func configMapUpdated(oldObj, newObj interface{}) {
	_, log := logger.GetNewContextWithLogger()
	oldFssConfigMap, ok := oldObj.(*v1.ConfigMap)
	if oldFssConfigMap == nil || !ok {
		log.Warnf("configMapUpdated: unrecognized old object %+v", oldObj)
		return
	}
	newFssConfigMap, ok := newObj.(*v1.ConfigMap)
	if newFssConfigMap == nil || !ok {
		log.Warnf("configMapUpdated: unrecognized new object %+v", newObj)
		return
	}
	// Check if there are updates to configmap data.
	if reflect.DeepEqual(newFssConfigMap.Data, oldFssConfigMap.Data) {
		log.Debug("configMapUpdated: No change in configmap data. Ignoring the event")
		return
	}

	if newFssConfigMap.Name == k8sOrchestratorInstance.supervisorFSS.configMapName &&
		newFssConfigMap.Namespace == k8sOrchestratorInstance.supervisorFSS.configMapNamespace {
		// The controller in nodes is not dependent on the supervisor FSS updates.
		if serviceMode == "node" {
			log.Debug("configMapUpdated: Ignoring supervisor FSS configmap update event in the nodes")
			return
		}
		// Ignore configmap updates if the cnscsisvfeaturestate CR is present in
		// supervisor namespace.
		if getSvFssCRAvailability() {
			log.Debugf("configMapUpdated: Ignoring supervisor FSS configmap update event as %q CR is present",
				featurestates.CRDSingular)
			return
		}
		// Update supervisor FSS.
		k8sOrchestratorInstance.supervisorFSS.featureStatesLock.Lock()
		k8sOrchestratorInstance.supervisorFSS.featureStates = newFssConfigMap.Data
		log.Warnf("configMapUpdated: Supervisor feature state values from %q stored successfully: %v",
			newFssConfigMap.Name, k8sOrchestratorInstance.supervisorFSS.featureStates)
		k8sOrchestratorInstance.supervisorFSS.featureStatesLock.Unlock()
	} else if newFssConfigMap.Name == k8sOrchestratorInstance.internalFSS.configMapName &&
		newFssConfigMap.Namespace == k8sOrchestratorInstance.internalFSS.configMapNamespace {
		// Update internal FSS.
		k8sOrchestratorInstance.internalFSS.featureStatesLock.Lock()
		k8sOrchestratorInstance.internalFSS.featureStates = newFssConfigMap.Data
		log.Warnf("configMapUpdated: Internal feature state values from %q stored successfully: %v",
			newFssConfigMap.Name, k8sOrchestratorInstance.internalFSS.featureStates)
		k8sOrchestratorInstance.internalFSS.featureStatesLock.Unlock()
	}
}

// configMapDeleted clears the feature state switch values from the feature
// states map.
func configMapDeleted(obj interface{}) {
	_, log := logger.GetNewContextWithLogger()
	fssConfigMap, ok := obj.(*v1.ConfigMap)
	if fssConfigMap == nil || !ok {
		log.Warnf("configMapDeleted: unrecognized object %+v", obj)
		return
	}
	// Check if it is either internal or supervisor FSS configmap.
	if fssConfigMap.Name == k8sOrchestratorInstance.supervisorFSS.configMapName &&
		fssConfigMap.Namespace == k8sOrchestratorInstance.supervisorFSS.configMapNamespace {
		if serviceMode == "node" {
			log.Debug("configMapDeleted: Ignoring supervisor FSS configmap delete event in the nodes")
			return
		}
		if getSvFssCRAvailability() {
			log.Debugf("configMapDeleted: Ignoring supervisor FSS configmap delete event as %q CR is present",
				featurestates.CRDSingular)
			return
		}
		log.Errorf("configMapDeleted: configMap %q in namespace %q deleted. "+
			"This is a system resource, kindly restore it.", fssConfigMap.Name, fssConfigMap.Namespace)
		os.Exit(1)
	} else if fssConfigMap.Name == k8sOrchestratorInstance.internalFSS.configMapName &&
		fssConfigMap.Namespace == k8sOrchestratorInstance.internalFSS.configMapNamespace {
		log.Errorf("configMapDeleted: configMap %q in namespace %q deleted. "+
			"This is a system resource, kindly restore it.", fssConfigMap.Name, fssConfigMap.Namespace)
		os.Exit(1)
	}
}

// fssCRAdded adds supervisor feature state switch values from the
// cnscsisvfeaturestate CR.
func fssCRAdded(obj interface{}) {
	_, log := logger.GetNewContextWithLogger()
	var svFSSObject featurestatesv1alpha1.CnsCsiSvFeatureStates
	err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.(*unstructured.Unstructured).Object, &svFSSObject)
	if err != nil {
		log.Errorf("fssCRAdded: failed to cast object to %s. err: %v", featurestates.CRDSingular, err)
		return
	}
	if svFSSObject.Name != featurestates.SVFeatureStateCRName {
		log.Warnf("fssCRAdded: Ignoring %s CR object with name %q", featurestates.CRDSingular, svFSSObject.Name)
		return
	}
	setSvFssCRAvailability(true)
	k8sOrchestratorInstance.supervisorFSS.featureStatesLock.Lock()
	for _, fss := range svFSSObject.Spec.FeatureStates {
		k8sOrchestratorInstance.supervisorFSS.featureStates[fss.Name] = strconv.FormatBool(fss.Enabled)
	}
	log.Infof("fssCRAdded: New supervisor feature states values stored successfully from %s CR object: %v",
		featurestates.SVFeatureStateCRName, k8sOrchestratorInstance.supervisorFSS.featureStates)
	k8sOrchestratorInstance.supervisorFSS.featureStatesLock.Unlock()
}

// fssCRUpdated updates supervisor feature state switch values from the
// cnscsisvfeaturestate CR.
func fssCRUpdated(oldObj, newObj interface{}) {
	_, log := logger.GetNewContextWithLogger()
	var (
		newSvFSSObject featurestatesv1alpha1.CnsCsiSvFeatureStates
		oldSvFSSObject featurestatesv1alpha1.CnsCsiSvFeatureStates
	)
	err := runtime.DefaultUnstructuredConverter.FromUnstructured(
		newObj.(*unstructured.Unstructured).Object, &newSvFSSObject)
	if err != nil {
		log.Errorf("fssCRUpdated: failed to cast new object to %s. err: %v", featurestates.CRDSingular, err)
		return
	}
	err = runtime.DefaultUnstructuredConverter.FromUnstructured(
		oldObj.(*unstructured.Unstructured).Object, &oldSvFSSObject)
	if err != nil {
		log.Errorf("fssCRUpdated: failed to cast old object to %s. err: %v", featurestates.CRDSingular, err)
		return
	}
	// Check if there are updates to the feature states in the
	// cnscsisvfeaturestate CR.
	if reflect.DeepEqual(oldSvFSSObject.Spec.FeatureStates, newSvFSSObject.Spec.FeatureStates) {
		log.Debug("fssCRUpdated: No change in %s CR data. Ignoring the event", featurestates.CRDSingular)
		return
	}

	if newSvFSSObject.Name != featurestates.SVFeatureStateCRName {
		log.Warnf("fssCRUpdated: Ignoring %s CR object with name %q", featurestates.CRDSingular, newSvFSSObject.Name)
		return
	}
	k8sOrchestratorInstance.supervisorFSS.featureStatesLock.Lock()
	for _, fss := range newSvFSSObject.Spec.FeatureStates {
		k8sOrchestratorInstance.supervisorFSS.featureStates[fss.Name] = strconv.FormatBool(fss.Enabled)
	}
	log.Warnf("fssCRUpdated: New supervisor feature states values stored successfully from %s CR object: %v",
		featurestates.SVFeatureStateCRName, k8sOrchestratorInstance.supervisorFSS.featureStates)
	k8sOrchestratorInstance.supervisorFSS.featureStatesLock.Unlock()
}

// fssCRDeleted crashes the container if the cnscsisvfeaturestate CR object
// with name svfeaturestates is deleted.
func fssCRDeleted(obj interface{}) {
	_, log := logger.GetNewContextWithLogger()
	var svFSSObject featurestatesv1alpha1.CnsCsiSvFeatureStates
	err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.(*unstructured.Unstructured).Object, &svFSSObject)
	if err != nil {
		log.Errorf("fssCRDeleted: failed to cast object to %s. err: %v", featurestates.CRDSingular, err)
		return
	}
	if svFSSObject.Name != featurestates.SVFeatureStateCRName {
		log.Warnf("fssCRDeleted: Ignoring %s CR object with name %q", featurestates.CRDSingular, svFSSObject.Name)
		return
	}
	setSvFssCRAvailability(false)
	// Logging an error here because cnscsisvfeaturestate CR should not be
	// deleted.
	log.Errorf("fssCRDeleted: %s CR object with name %q in namespace %q deleted. "+
		"This is a system resource, kindly restore it.",
		featurestates.CRDSingular, svFSSObject.Name, svFSSObject.Namespace)
	os.Exit(1)
}

// initVolumeHandleToPvcMap performs all the operations required to initialize
// the volume id to PVC name map. It also watches for PV update & delete
// operations, and updates the map accordingly.
func initVolumeHandleToPvcMap(ctx context.Context, controllerClusterFlavor cnstypes.CnsClusterFlavor) error {
	log := logger.GetLogger(ctx)
	log.Debugf("Initializing volume ID to PVC name map")
	k8sOrchestratorInstance.volumeIDToPvcMap = &volumeIDToPvcMap{
		RWMutex: &sync.RWMutex{},
		items:   make(map[string]string),
	}

	k8sOrchestratorInstance.pvcToVolumeIDMap = &pvcToVolumeIDMap{
		RWMutex: &sync.RWMutex{},
		items:   make(map[string]string),
	}

	k8sOrchestratorInstance.volumeIDToNameMap = &volumeIDToNameMap{
		RWMutex: &sync.RWMutex{},
		items:   make(map[string]string),
	}

	// Set up kubernetes resource listener to listen events on PersistentVolumes
	// and PersistentVolumeClaims.
	if (controllerClusterFlavor == cnstypes.CnsClusterFlavorVanilla && serviceMode != "node") ||
		(controllerClusterFlavor == cnstypes.CnsClusterFlavorWorkload) {

		err := k8sOrchestratorInstance.informerManager.AddPVListener(
			ctx,
			func(obj interface{}) { // Add.
				pvAdded(obj)
			},
			func(oldObj interface{}, newObj interface{}) { // Update.
				pvUpdated(oldObj, newObj)
			},
			func(obj interface{}) { // Delete.
				pvDeleted(obj)
			})
		if err != nil {
			return logger.LogNewErrorf(log, "failed to listen on PVs. Error: %v", err)
		}

		err = k8sOrchestratorInstance.informerManager.AddPVCListener(
			ctx,
			func(obj interface{}) { // Add.
				pvcAdded(obj)
			},
			nil, // Update.
			nil, // Delete.
		)
		if err != nil {
			return logger.LogNewErrorf(log, "failed to listen on PVCs. Error: %v", err)
		}
	}
	return nil
}

// Since informerManager's sharedInformerFactory is started with no resync
// period, it never syncs the existing cluster objects to its Store when
// it's started. pvcAdded provides no additional handling but it ensures that
// existing PVCs in the cluster gets added to sharedInformerFactory's Store
// before it's started. Then using informerManager's PVCLister should find
// the existing PVCs as well.
func pvcAdded(obj interface{}) {}

// pvAdded adds a volume to the volumeIDToPvcMap and  pvcToVolumeIDMap if it's already in Bound phase.
// This ensures that all existing PVs in the cluster are added to the map, even
// across container restarts.
func pvAdded(obj interface{}) {
	_, log := logger.GetNewContextWithLogger()
	pv, ok := obj.(*v1.PersistentVolume)
	if pv == nil || !ok {
		log.Warnf("pvAdded: unrecognized object %+v", obj)
		return
	}

	if pv.Spec.CSI != nil && pv.Spec.CSI.Driver == csitypes.Name {
		if !isFileVolume(pv) && pv.Spec.ClaimRef != nil && pv.Status.Phase == v1.VolumeBound {
			// We should not be caching file volumes to the map.
			// Add volume handle to PVC mapping.
			objKey := pv.Spec.CSI.VolumeHandle
			objVal := pv.Spec.ClaimRef.Namespace + "/" + pv.Spec.ClaimRef.Name

			k8sOrchestratorInstance.volumeIDToPvcMap.add(objKey, objVal)
			k8sOrchestratorInstance.pvcToVolumeIDMap.add(objVal, objKey)
			log.Debugf("pvAdded: Added '%s and %s' mapping to volumeIDToPvcMap and pvcToVolumeIDMap", objKey, objVal)
		}
		k8sOrchestratorInstance.volumeIDToNameMap.add(pv.Spec.CSI.VolumeHandle, pv.Name)
		log.Debugf("pvAdded: Added '%s -> %s' pair to volumeIDToNameMap", pv.Spec.CSI.VolumeHandle, pv.Name)
	}
	// Add VCP-CSI migrated volumes to the volumeIDToNameMap map.
	// Since cns query will return all the volumes including the migrated ones, the map would need to be a
	// union of migrated VCP-CSI volumes and CSI volumes, as well.
	if pv.Spec.VsphereVolume != nil &&
		k8sOrchestratorInstance.IsFSSEnabled(context.Background(), common.CSIMigration) &&
		isValidMigratedvSphereVolume(context.Background(), pv.ObjectMeta) {
		if pv.Status.Phase == v1.VolumeBound {
			k8sOrchestratorInstance.volumeIDToNameMap.add(pv.Spec.VsphereVolume.VolumePath, pv.Name)
			log.Debugf("Migrated pvAdded: Added '%s -> %s' pair to volumeIDToNameMap", pv.Spec.VsphereVolume.VolumePath, pv.Name)
		}
	}
}

// pvUpdated updates the volumeIDToPvcMap and pvcToVolumeIDMap when a PV goes to Bound phase.
func pvUpdated(oldObj, newObj interface{}) {
	_, log := logger.GetNewContextWithLogger()
	// Get old and new PV objects.
	oldPv, ok := oldObj.(*v1.PersistentVolume)
	if oldPv == nil || !ok {
		log.Warnf("PVUpdated: unrecognized old object %+v", oldObj)
		return
	}

	newPv, ok := newObj.(*v1.PersistentVolume)
	if newPv == nil || !ok {
		log.Warnf("PVUpdated: unrecognized new object %+v", newObj)
		return
	}

	// PV goes into Bound phase.
	if oldPv.Status.Phase != v1.VolumeBound && newPv.Status.Phase == v1.VolumeBound {
		if newPv.Spec.CSI != nil && newPv.Spec.CSI.Driver == csitypes.Name &&
			newPv.Spec.ClaimRef != nil {
			if !isFileVolume(newPv) {

				log.Debugf("pvUpdated: PV %s went to Bound phase", newPv.Name)
				// Add volume handle to PVC mapping.
				objKey := newPv.Spec.CSI.VolumeHandle
				objVal := newPv.Spec.ClaimRef.Namespace + "/" + newPv.Spec.ClaimRef.Name

				k8sOrchestratorInstance.volumeIDToPvcMap.add(objKey, objVal)
				k8sOrchestratorInstance.pvcToVolumeIDMap.add(objVal, objKey)
				log.Debugf("pvUpdated: Added '%s and %s' mapping to pvcToVolumeIDMap and pvcToVolumeID",
					objKey, objVal)
			}
			k8sOrchestratorInstance.volumeIDToNameMap.add(newPv.Spec.CSI.VolumeHandle, newPv.Name)
			log.Debugf("pvUpdated: Added '%s -> %s' pair to volumeIDToNameMap", newPv.Spec.CSI.VolumeHandle, newPv.Name)
		}
	}

	// Update VCP-CSI migrated volumes to the volumeIDToNameMap map.
	// Since cns query will return all the volumes including the migrated ones, the map would need to be a
	// union of migrated VCP-CSI volumes and CSI volumes, as well.
	if newPv.Spec.VsphereVolume != nil &&
		k8sOrchestratorInstance.IsFSSEnabled(context.Background(), common.CSIMigration) &&
		isValidMigratedvSphereVolume(context.Background(), newPv.ObjectMeta) {
		if oldPv.Status.Phase != v1.VolumeBound && newPv.Status.Phase == v1.VolumeBound {
			k8sOrchestratorInstance.volumeIDToNameMap.add(newPv.Spec.VsphereVolume.VolumePath, newPv.Name)
			log.Debugf("Migrated pvUpdated: Added '%s -> %s' pair to volumeIDToNameMap",
				newPv.Spec.VsphereVolume.VolumePath, newPv.Name)
		}
	}
}

// pvDeleted deletes an entry from volumeIDToPvcMap and pvcToVolumeIDMap when a PV gets deleted.
func pvDeleted(obj interface{}) {
	_, log := logger.GetNewContextWithLogger()
	pv, ok := obj.(*v1.PersistentVolume)
	if pv == nil || !ok {
		log.Warnf("PVDeleted: unrecognized object %+v", obj)
		return
	}
	log.Debugf("PV: %s deleted. Removing entry from volumeIDToPvcMap and pvcToVolumeIDMap", pv.Name)

	if pv.Spec.CSI != nil && pv.Spec.CSI.Driver == csitypes.Name {
		k8sOrchestratorInstance.volumeIDToPvcMap.remove(pv.Spec.CSI.VolumeHandle)
		log.Debugf("k8sorchestrator: Deleted key %s from volumeIDToPvcMap", pv.Spec.CSI.VolumeHandle)
		k8sOrchestratorInstance.volumeIDToNameMap.remove(pv.Spec.CSI.VolumeHandle)
		log.Debugf("k8sorchestrator: Deleted key %s from volumeIDToNameMap", pv.Spec.CSI.VolumeHandle)
		k8sOrchestratorInstance.pvcToVolumeIDMap.remove(pv.Spec.ClaimRef.Namespace + "/" + pv.Spec.ClaimRef.Name)
		log.Debugf("k8sorchestrator: Deleted key %s from pvcToVolumeID",
			pv.Spec.ClaimRef.Namespace+"/"+pv.Spec.ClaimRef.Name)
	}
	if pv.Spec.VsphereVolume != nil && k8sOrchestratorInstance.IsFSSEnabled(context.Background(), common.CSIMigration) {
		k8sOrchestratorInstance.volumeIDToNameMap.remove(pv.Spec.VsphereVolume.VolumePath)
		log.Debugf("k8sorchestrator migrated volume: Deleted key %s from volumeIDToNameMap",
			pv.Spec.VsphereVolume.VolumePath)
	}

}

// GetAllK8sVolumes returns list of volumes in a bound state
// list Includes Migrated vSphere Volumes VMDK Paths for in-tree vSphere PVs and Volume IDs for CSI PVs
func (c *K8sOrchestrator) GetAllK8sVolumes() []string {
	volumeIDs := make([]string, 0)
	for volumeID := range c.volumeIDToNameMap.items {
		volumeIDs = append(volumeIDs, volumeID)
	}
	return volumeIDs
}

// HandleEnablementOfWLDICapability starts a ticker and checks after every 2 minutes if
// Workload_Domain_Isolation_Supported capability is enabled in capabilities CR or not.
// If this capability was disabled and now got enabled, then container will be restarted.
func HandleEnablementOfWLDICapability(ctx context.Context, clusterFlavor cnstypes.CnsClusterFlavor,
	gcEndpoint, gcPort string) {
	log := logger.GetLogger(ctx)
	var restClientConfig *restclient.Config
	var err error

	if clusterFlavor == cnstypes.CnsClusterFlavorWorkload {
		restClientConfig, err = clientconfig.GetConfig()
		if err != nil {
			log.Errorf("failed to get Kubernetes config. Err: %+v", err)
			os.Exit(1)
		}
	} else if clusterFlavor == cnstypes.CnsClusterFlavorGuest {
		restClientConfig = k8s.GetRestClientConfigForSupervisor(ctx,
			gcEndpoint, gcPort)
		for {
			// If supervisor is old but TKR is new, it could happen that Capabilities CR is not
			// registered on the supervisor. So, before starting a ticker check if Capabilities
			// CR is registered on the supervisor.
			apiextensionsClientSet, err := apiextensionsclientset.NewForConfig(restClientConfig)
			if err != nil {
				log.Errorf("failed to create apiextension clientset using config. Err: %+v", err)
				os.Exit(1)
			}
			_, err = apiextensionsClientSet.ApiextensionsV1().CustomResourceDefinitions().Get(ctx,
				"capabilities.iaas.vmware.com", metav1.GetOptions{})
			if err != nil {
				if apierrors.IsNotFound(err) {
					// If capabilities CR is not registered on supervisor, then sleep for some time and check
					// again if CR has been registered on supervisor. If TKR is new, but supervisor is old, then
					// it could happen that capabilities CR is not registered on the supervisor cluster.
					// But when supervisor cluster is upgraded, capabilities CR might get registered and in that
					// case we have to start a ticker to watch on capability value changes.
					log.Infof("CR instance capabilities.iaas.vmware.com is not registered on supervisor, " +
						"sleep for some time and check again if the CR instance is registered " +
						"on the supervisor cluster.")
					time.Sleep(10 * time.Minute)
					continue
				} else {
					log.Errorf("failed to check if Capabilities CR is registered. Err: %v", err)
					os.Exit(1)
				}
			}
			break
		}
	}

	wcpCapabilityApiClient, err := k8s.NewClientForGroup(ctx, restClientConfig, wcpcapapis.GroupName)
	if err != nil {
		log.Errorf("failed to create wcpCapabilityApi client. Err: %+v", err)
		os.Exit(1)
	}
	ticker := time.NewTicker(time.Duration(2) * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		err := SetWcpCapabilitiesMap(ctx, wcpCapabilityApiClient)
		if err != nil {
			log.Errorf("failed to set WCP capabilities map, Err: %+v", err)
			os.Exit(1)
		}
		log.Debugf("WCP cluster capabilities map - %+v", WcpCapabilitiesMap)

		fssVal := WcpCapabilitiesMap[common.WorkloadDomainIsolation]
		if fssVal {
			log.Infof("%s capability has been enabled in capabilities CR %s. "+
				"Restarting the container as capability has changed from false to true.",
				common.WorkloadDomainIsolation, common.WCPCapabilitiesCRName)
			os.Exit(1)
		}
	}
}

// SetWcpCapabilitiesMap reads the capabilities values from 'supervisor-capabilities' CR in
// supervisor cluster and sets values in global wcp capabilities map.
func SetWcpCapabilitiesMap(ctx context.Context, wcpCapabilityApiClient client.Client) error {
	log := logger.GetLogger(ctx)
	// Check the 'supervisor-capabilities' CR in supervisor
	wcpCapabilities := &wcpcapv1alph1.Capabilities{}
	err := wcpCapabilityApiClient.Get(ctx, k8stypes.NamespacedName{
		Name: common.WCPCapabilitiesCRName},
		wcpCapabilities)
	if err != nil {
		log.Errorf("failed to fetch Capabilities CR instance "+
			" with name %q Error: %+v", common.WCPCapabilitiesCRName, err)
		return err
	}

	wcpCapabilitiesMapMutex.Lock()
	defer wcpCapabilitiesMapMutex.Unlock()
	if WcpCapabilitiesMap == nil {
		WcpCapabilitiesMap = make(map[string]bool)
	}
	for capName, capStatus := range wcpCapabilities.Status.Supervisor {
		WcpCapabilitiesMap[string(capName)] = capStatus.Activated
	}
	return nil
}

// IsFSSEnabled utilises the cluster flavor to check their corresponding FSS
// maps and returns if the feature state switch is enabled for the given feature
// indicated by featureName.
func (c *K8sOrchestrator) IsFSSEnabled(ctx context.Context, featureName string) bool {
	log := logger.GetLogger(ctx)
	var (
		internalFeatureState   bool
		supervisorFeatureState bool
		err                    error
	)
	if c.clusterFlavor == cnstypes.CnsClusterFlavorVanilla {
		// first check hard coded FSS map. these are GA'ed features
		// we don't need a lock for this one as this is map is read only after init
		if _, isReleased := c.releasedVanillaFSS[featureName]; isReleased {
			return true
		}

		c.internalFSS.featureStatesLock.RLock()
		// for testing, we still need to provide a way to toggle unreleased fss.
		// so we can look in the live configmap for these
		if state, ok := c.internalFSS.featureStates[featureName]; ok {
			c.internalFSS.featureStatesLock.RUnlock()
			internalFeatureState, err = strconv.ParseBool(state)
			if err != nil {
				log.Errorf("Error while converting %v feature state value: %v to boolean. "+
					"Setting the feature state to false", featureName, internalFeatureState)
				return false
			}
			return internalFeatureState
		}
		c.internalFSS.featureStatesLock.RUnlock()
		log.Infof("Could not find the %s feature state in ConfigMap %s. "+
			"Setting the feature state to false", featureName, c.internalFSS.configMapName)
		return false
	} else if c.clusterFlavor == cnstypes.CnsClusterFlavorWorkload {
		// Check if it is WCP defined feature state.
		if _, exists := common.WCPFeatureStates[featureName]; exists {
			log.Infof("Feature %q is a WCP defined feature state. Reading the capabilities CR %q.",
				featureName, common.WCPCapabilitiesCRName)

			if len(WcpCapabilitiesMap) == 0 {
				restConfig, err := clientconfig.GetConfig()
				if err != nil {
					log.Errorf("failed to get Kubernetes config. Err: %+v", err)
					return false
				}
				wcpCapabilityApiClient, err := k8s.NewClientForGroup(ctx, restConfig, wcpcapapis.GroupName)
				if err != nil {
					log.Errorf("failed to create wcpCapabilityApi client. Err: %+v", err)
					return false
				}
				err = SetWcpCapabilitiesMap(ctx, wcpCapabilityApiClient)
				if err != nil {
					log.Errorf("failed to set WCP capabilities map, Err: %+v", err)
					return false
				}
				log.Infof("WCP cluster capabilities map - %+v", WcpCapabilitiesMap)
			}
			if supervisorFeatureState, exists := WcpCapabilitiesMap[featureName]; exists {
				log.Infof("Supervisor capability %q is set to %t", featureName, supervisorFeatureState)

				if !supervisorFeatureState {
					// if capability can be enabled after upgrading CSI, we need to fetch capabilities CR again and
					// confirm FSS is still disabled, or it got enabled.
					// WCPFeatureStatesSupportsLateEnablement contains capabilities which can be enabled later after
					// CSI is upgraded and up and running.
					if _, exists = common.WCPFeatureStatesSupportsLateEnablement[featureName]; exists {
						restConfig, err := clientconfig.GetConfig()
						if err != nil {
							log.Errorf("failed to get Kubernetes config. Err: %+v", err)
							return false
						}
						wcpCapabilityApiClient, err := k8s.NewClientForGroup(ctx, restConfig, wcpcapapis.GroupName)
						if err != nil {
							log.Errorf("failed to create wcpCapabilityApi client. Err: %+v", err)
							return false
						}
						err = SetWcpCapabilitiesMap(ctx, wcpCapabilityApiClient)
						if err != nil {
							log.Errorf("failed to set WCP capabilities map, Err: %+v", err)
							return false
						}
						log.Debugf("WCP cluster capabilities map - %+v", WcpCapabilitiesMap)

						if supervisorFeatureState, exists = WcpCapabilitiesMap[featureName]; exists {
							log.Debugf("Supervisor capability %q was disabled, "+
								"now it is set to %t", featureName, supervisorFeatureState)
							if supervisorFeatureState {
								log.Infof("Supervisor capabilty %q was disabled, but now it has been enabled.",
									featureName)
							}
						}
					}
				}
				return supervisorFeatureState
			}
			return false
		}

		// Check SV FSS map.
		c.supervisorFSS.featureStatesLock.RLock()
		if flag, ok := c.supervisorFSS.featureStates[featureName]; ok {
			c.supervisorFSS.featureStatesLock.RUnlock()
			supervisorFeatureState, err = strconv.ParseBool(flag)
			if err != nil {
				log.Errorf("Error while converting %v feature state value: %v to boolean. "+
					"Setting the feature state to false", featureName, supervisorFeatureState)
				return false
			}
			return supervisorFeatureState
		}
		c.supervisorFSS.featureStatesLock.RUnlock()
		log.Infof("Could not find the %s feature state in ConfigMap %s. "+
			"Setting the feature state to false", featureName, c.supervisorFSS.configMapName)
		return false
	} else if c.clusterFlavor == cnstypes.CnsClusterFlavorGuest {
		isPVCSIFSSEnabled := c.IsPVCSIFSSEnabled(ctx, featureName)
		if isPVCSIFSSEnabled {
			// Skip SV FSS check for Windows Support since there is no dependency on supervisor
			if featureName == common.CSIWindowsSupport {
				log.Info("CSI Windows Suppport is set to true in pvcsi fss configmap. Skipping SV FSS check")
				return true
			}

			// If PVCSI FSS has associated WCP capability in supervisor cluster, then check if that WCP
			// capability is enabled or disabled by fetching its value from capabilities CR on supervisor.
			if wcpFeatureState, exists := common.WCPFeatureStateAssociatedWithPVCSI[featureName]; exists {
				if len(WcpCapabilitiesMap) == 0 {
					// Read capabilities CR from supervisor cluster
					cfg, err := cnsconfig.GetConfig(ctx)
					if err != nil {
						log.Errorf("failed to read config. Error: %+v", err)
						return false
					}
					// Get rest client config for supervisor.
					restClientConfig := k8s.GetRestClientConfigForSupervisor(ctx, cfg.GC.Endpoint, cfg.GC.Port)
					wcpCapabilityApiClient, err := k8s.NewClientForGroup(ctx, restClientConfig, wcpcapapis.GroupName)
					if err != nil {
						log.Errorf("failed to create wcpCapabilityApi client. Err: %+v", err)
						return false
					}
					err = SetWcpCapabilitiesMap(ctx, wcpCapabilityApiClient)
					if err != nil {
						log.Errorf("failed to set WCP capabilities map, Err: %+v", err)
						return false
					}
					log.Infof("WCP cluster capabilities map - %+v", WcpCapabilitiesMap)
				}

				if supervisorFeatureState, exists := WcpCapabilitiesMap[wcpFeatureState]; exists {
					log.Debugf("Supervisor capability %q is set to %t", wcpFeatureState,
						supervisorFeatureState)
					return supervisorFeatureState
				}
				return false
			}

			return c.IsCNSCSIFSSEnabled(ctx, featureName)
		}
		return false
	}
	log.Debugf("cluster flavor %q not recognised. Defaulting to false", c.clusterFlavor)
	return false
}

// IsCNSCSIFSSEnabled checks if Feature is enabled in CNSCSI and returns true if enabled
func (c *K8sOrchestrator) IsCNSCSIFSSEnabled(ctx context.Context, featureName string) bool {
	var supervisorFeatureState bool
	var err error
	log := logger.GetLogger(ctx)
	// Check SV FSS map.
	c.supervisorFSS.featureStatesLock.RLock()
	if flag, ok := c.supervisorFSS.featureStates[featureName]; ok {
		c.supervisorFSS.featureStatesLock.RUnlock()
		supervisorFeatureState, err = strconv.ParseBool(flag)
		if err != nil {
			log.Errorf("Error while converting %v feature state value: %v to boolean. "+
				"Setting the feature state to false", featureName, supervisorFeatureState)
			return false
		}
		if !supervisorFeatureState {
			// If FSS set to false, return.
			log.Infof("%s feature state is set to false in %s ConfigMap", featureName, c.supervisorFSS.configMapName)
			return supervisorFeatureState
		}
	} else {
		c.supervisorFSS.featureStatesLock.RUnlock()
		log.Infof("Could not find the %s feature state in ConfigMap %s. Setting the feature state to false",
			featureName, c.supervisorFSS.configMapName)
		return false
	}
	return true
}

// IsPVCSIFSSEnabled checks if Feature is enabled in PVCSI and returns true if enabled
func (c *K8sOrchestrator) IsPVCSIFSSEnabled(ctx context.Context, featureName string) bool {
	var internalFeatureState bool
	var err error
	log := logger.GetLogger(ctx)
	c.internalFSS.featureStatesLock.RLock()
	if flag, ok := c.internalFSS.featureStates[featureName]; ok {
		c.internalFSS.featureStatesLock.RUnlock()
		internalFeatureState, err = strconv.ParseBool(flag)
		if err != nil {
			log.Errorf("Error while converting %v feature state value: %v to boolean. "+
				"Setting the feature state to false", featureName, internalFeatureState)
			return false
		}
		if !internalFeatureState {
			// If FSS set to false, return.
			log.Infof("%s feature state set to false in %s ConfigMap", featureName, c.internalFSS.configMapName)
			return internalFeatureState
		}
	} else {
		c.internalFSS.featureStatesLock.RUnlock()
		log.Infof("Could not find the %s feature state in ConfigMap %s. Setting the feature state to false",
			featureName, c.internalFSS.configMapName)
		return false
	}
	return true
}

// EnableFSS helps enable feature state switch in the FSS config map
func (c *K8sOrchestrator) EnableFSS(ctx context.Context, featureName string) error {
	log := logger.GetLogger(ctx)
	return logger.LogNewErrorCode(log, codes.Unimplemented,
		"EnableFSS is not implemented.")
}

// DisableFSS helps disable feature state switch in the FSS config map
func (c *K8sOrchestrator) DisableFSS(ctx context.Context, featureName string) error {
	log := logger.GetLogger(ctx)
	return logger.LogNewErrorCode(log, codes.Unimplemented,
		"DisableFSS is not implemented.")
}

// GetPvcObjectByName returns PVC object for the given pvc name in the said namespace.
func (c *K8sOrchestrator) GetPvcObjectByName(ctx context.Context, pvcName string,
	namespace string) (*v1.PersistentVolumeClaim, error) {
	log := logger.GetLogger(ctx)
	pvcObj, err := c.informerManager.GetPVCLister().PersistentVolumeClaims(namespace).Get(pvcName)
	if err != nil {
		log.Errorf("failed to get pvc: %s in namespace: %s. err=%v", pvcName, namespace, err)
		return nil, err
	}
	return pvcObj, nil

}

// IsFakeAttachAllowed checks if the volume is eligible to be fake attached
// and returns a bool value.
func (c *K8sOrchestrator) IsFakeAttachAllowed(ctx context.Context, volumeID string,
	volumeManager cnsvolume.Manager) (bool, error) {
	log := logger.GetLogger(ctx)
	// Check pvc annotations.
	pvcAnn, err := c.getPVCAnnotations(ctx, volumeID)
	if err != nil {
		log.Errorf("IsFakeAttachAllowed: failed to get pvc annotations for volume ID %s "+
			"while checking eligibility for fake attach", volumeID)
		return false, err
	}

	if val, found := pvcAnn[common.AnnIgnoreInaccessiblePV]; found && val == "yes" {
		log.Debugf("Found %s annotation on pvc set to yes for volume: %s. Checking volume health on CNS volume.",
			common.AnnIgnoreInaccessiblePV, volumeID)
		// Check if volume is inaccessible.
		querySelection := cnstypes.CnsQuerySelection{
			Names: []string{string(cnstypes.QuerySelectionNameTypeHealthStatus)},
		}
		vol, err := common.QueryVolumeByID(ctx, volumeManager, volumeID, &querySelection)
		if err != nil {
			log.Errorf("failed to query CNS for volume ID %s while checking eligibility for fake attach", volumeID)
			return false, err
		}

		if vol.HealthStatus != string(pbmtypes.PbmHealthStatusForEntityUnknown) {
			volHealthStatus, err := common.ConvertVolumeHealthStatus(ctx, vol.VolumeId.Id, vol.HealthStatus)
			if err != nil {
				log.Errorf("invalid health status: %s for volume: %s", vol.HealthStatus, vol.VolumeId.Id)
				return false, err
			}
			log.Debugf("CNS volume health is: %s", volHealthStatus)

			// If volume is inaccessible, it can be fake attached.
			if volHealthStatus == common.VolHealthStatusInaccessible {
				log.Infof("Volume: %s is eligible to be fake attached", volumeID)
				return true, nil
			}
		}
		// For all other cases, return false.
		return false, nil
	}
	// Annotation is not found or not set to true, return false.
	log.Debugf("Annotation %s not found or not set to true on pvc for volume %s",
		common.AnnIgnoreInaccessiblePV, volumeID)
	return false, nil
}

// MarkFakeAttached updates the pvc corresponding to volume to have a fake
// attach annotation.
func (c *K8sOrchestrator) MarkFakeAttached(ctx context.Context, volumeID string) error {
	log := logger.GetLogger(ctx)
	annotations := make(map[string]string)
	annotations[common.AnnVolumeHealth] = common.VolHealthStatusInaccessible
	annotations[common.AnnFakeAttached] = "yes"

	// Update annotations.
	// Along with updating fake attach annotation on pvc, also update the volume
	// health on pvc as inaccessible, as that's one of the conditions for volume
	// to be fake attached.
	if err := c.updatePVCAnnotations(ctx, volumeID, annotations); err != nil {
		log.Errorf("failed to mark fake attach annotation on the pvc for volume %s. Error:%+v", volumeID, err)
		return err
	}

	return nil
}

// ClearFakeAttached checks if pvc corresponding to the volume has fake
// annotations, and unmark it as not fake attached.
func (c *K8sOrchestrator) ClearFakeAttached(ctx context.Context, volumeID string) error {
	log := logger.GetLogger(ctx)
	// Check pvc annotations.
	pvcAnn, err := c.getPVCAnnotations(ctx, volumeID)
	if err != nil {
		if err.Error() == common.ErrNotFound.Error() {
			// PVC not found, which means PVC could have been deleted. No need to proceed.
			return nil
		}
		log.Errorf("ClearFakeAttached: failed to get pvc annotations for volume ID %s "+
			"while checking if it was fake attached", volumeID)
		return err
	}
	val, found := pvcAnn[common.AnnFakeAttached]
	if found && val == "yes" {
		log.Debugf("Volume: %s was fake attached", volumeID)
		// Clear the fake attach annotation.
		annotations := make(map[string]string)
		annotations[common.AnnFakeAttached] = ""
		if err := c.updatePVCAnnotations(ctx, volumeID, annotations); err != nil {
			if err.Error() == common.ErrNotFound.Error() {
				// PVC not found, which means PVC could have been deleted.
				return nil
			}
			log.Errorf("failed to clear fake attach annotation on the pvc for volume %s. Error:%+v", volumeID, err)
			return err
		}
	}
	return nil
}

// initVolumeNameToNodesMap performs all the operations required to initialize
// the PVName to node names map. It also watches for volume attachment add,
// update & delete operations, and updates the map accordingly.
func initVolumeNameToNodesMap(ctx context.Context, controllerClusterFlavor cnstypes.CnsClusterFlavor) error {
	log := logger.GetLogger(ctx)
	log.Debugf("Initializing volumeName/pvName to node name map")
	k8sOrchestratorInstance.volumeNameToNodesMap = &volumeNameToNodesMap{
		RWMutex: &sync.RWMutex{},
		items:   make(map[string][]string),
	}

	// Set up kubernetes resource listener to listen events on volume attachments
	if (controllerClusterFlavor == cnstypes.CnsClusterFlavorVanilla && serviceMode != "node") ||
		(controllerClusterFlavor == cnstypes.CnsClusterFlavorWorkload) {

		err := k8sOrchestratorInstance.informerManager.AddVolumeAttachmentListener(
			ctx,
			func(obj interface{}) { // Add.
				volumeAttachmentAdded(obj)
			},
			func(oldObj interface{}, newObj interface{}) { // Update
				volumeAttachmentUpdated(oldObj, newObj)
			},
			func(obj interface{}) { // Delete.
				volumeAttachmentDeleted(obj)
			})
		if err != nil {
			return logger.LogNewErrorf(log, "failed to listen on volume attachment instances. Error: %v", err)
		}
	}
	return nil
}

// volumeAttachmentAdded adds a new entry or updates an existing entry
// in the volumeIDToNodeNames map if the volume attachment status is
// true
func volumeAttachmentAdded(obj interface{}) {
	log := logger.GetLogger(context.Background())
	volAttach, ok := obj.(*storagev1.VolumeAttachment)
	if volAttach == nil || !ok {
		log.Warnf("volumeAttachmentAdded: unrecognized object %+v", obj)
		return
	}
	if volAttach.Spec.Attacher != csitypes.Name {
		return
	}
	log.Debugf("volumeAttachmentAdded: volume=%v", volAttach)
	if volAttach.Status.Attached {
		if volAttach.Spec.Source.PersistentVolumeName == nil {
			// return for inline volume
			return
		}
		volumeName := *volAttach.Spec.Source.PersistentVolumeName
		nodeName := volAttach.Spec.NodeName
		nodes := k8sOrchestratorInstance.volumeNameToNodesMap.get(volumeName)
		found := false
		for _, node := range nodes {
			if node == nodeName {
				found = true
				break
			}
		}
		if !found {
			nodes = append(nodes, nodeName)
			log.Debugf("volumeAttachmentAdded: Adding nodeName %s to volumeID %s:%v map", nodeName, volumeName, nodes)
			k8sOrchestratorInstance.volumeNameToNodesMap.add(volumeName, nodes)
		}
	}
}

// volumeAttachmentUpdated updates an existing entry in the volumeIDToNodeNames map
// if the volume attachment status is true
func volumeAttachmentUpdated(oldObj, newObj interface{}) {
	log := logger.GetLogger(context.Background())
	oldVolAttach, ok := oldObj.(*storagev1.VolumeAttachment)
	if oldVolAttach == nil || !ok {
		log.Warnf("volumeAttachmentUpdated: unrecognized old object %+v", oldObj)
		return
	}

	newVolAttach, ok := newObj.(*storagev1.VolumeAttachment)
	if newVolAttach == nil || !ok {
		log.Warnf("volumeAttachmentUpdated: unrecognized new object %+v", newObj)
		return
	}

	if newVolAttach.Spec.Attacher != csitypes.Name {
		return
	}

	if !oldVolAttach.Status.Attached && newVolAttach.Status.Attached {
		if newVolAttach.Spec.Source.PersistentVolumeName == nil {
			// return for inline volume
			return
		}
		volumeName := *newVolAttach.Spec.Source.PersistentVolumeName
		nodeName := newVolAttach.Spec.NodeName
		nodes := k8sOrchestratorInstance.volumeNameToNodesMap.get(volumeName)
		found := false
		for _, node := range nodes {
			if node == nodeName {
				found = true
				break
			}
		}
		if !found {
			nodes = append(nodes, nodeName)
			log.Debugf("volumeAttachmentUpdated: Adding nodeName %s to volumeID %s:%v map",
				nodeName, volumeName, nodes)
			k8sOrchestratorInstance.volumeNameToNodesMap.add(volumeName, nodes)
		}
	}
}

// volumeAttachmentDeleted deletes an entry or removes node name form an
// existing entry in the volumeIDToNodeNames map if the volume attachment
// status is false
func volumeAttachmentDeleted(obj interface{}) {
	log := logger.GetLogger(context.Background())
	volAttach, ok := obj.(*storagev1.VolumeAttachment)
	if volAttach == nil || !ok {
		log.Warnf("volumeAttachmentDeleted: unrecognized object %+v", obj)
		return
	}
	if volAttach.Spec.Attacher != csitypes.Name {
		return
	}
	if !volAttach.Status.Attached {
		log.Debugf("volumeAttachmentDeleted: volume attachment deleted: volume=%v", volAttach)
		if volAttach.Spec.Source.PersistentVolumeName == nil {
			// return for inline volume
			return
		}
		volumeName := *volAttach.Spec.Source.PersistentVolumeName

		nodeName := volAttach.Spec.NodeName
		nodes := k8sOrchestratorInstance.volumeNameToNodesMap.get(volumeName)
		found := false
		for i, node := range nodes {
			if node == nodeName {
				nodes = append(nodes[:i], nodes[i+1:]...)
				found = true
				break
			}
		}
		if found {
			log.Debugf("volumeAttachmentDeleted: Deleting nodeName %s to volumeName %s map",
				nodeName, volumeName)
			if len(nodes) == 0 {
				k8sOrchestratorInstance.volumeNameToNodesMap.remove(volumeName)
			} else {
				k8sOrchestratorInstance.volumeNameToNodesMap.add(volumeName, nodes)
			}
		}
	}
}

// GetNodesForVolumes returns a map containing the volumeID to node names map for the given
// list of volumeIDs
func (c *K8sOrchestrator) GetNodesForVolumes(ctx context.Context, volumeIDs []string) map[string][]string {
	volumeIDToNodeNames := make(map[string][]string)
	for _, volumeID := range volumeIDs {
		volumeName, found := c.volumeIDToNameMap.get(volumeID)
		if found {
			volumeIDToNodeNames[volumeID] = c.volumeNameToNodesMap.get(volumeName)
		}

	}
	return volumeIDToNodeNames
}

// initNodeIDToNameMap performs all the operations required to initialize
// the node ID to  name map. It also watches for node add, update & delete
// operations, and updates the map accordingly.
func initNodeIDToNameMap(ctx context.Context) error {
	log := logger.GetLogger(ctx)

	log.Debugf("Initializing node ID to node name map")
	k8sOrchestratorInstance.nodeIDToNameMap = &nodeIDToNameMap{
		RWMutex: &sync.RWMutex{},
		items:   make(map[string]string),
	}

	// Set up kubernetes resource listener to listen events on Node
	err := k8sOrchestratorInstance.informerManager.AddNodeListener(
		ctx,
		func(obj interface{}) { // Add.
			nodeAdd(obj)
		},
		func(oldObj interface{}, newObj interface{}) { // Update.
			nodeUpdate(oldObj, newObj)
		},
		func(obj interface{}) { // Delete.
			nodeRemove(obj)
		})
	if err != nil {
		return logger.LogNewErrorf(log, "failed to listen on nodes. Error: %v", err)
	}
	return nil
}

// nodeAdd adds an entry into nodeIDToNameMap. The node MoID is retrieved from the
// node annotation vmware-system-esxi-node-moid
func nodeAdd(obj interface{}) {
	log := logger.GetLogger(context.Background())
	node, ok := obj.(*v1.Node)
	if node == nil || !ok {
		log.Warnf("nodeAdd: unrecognized object %+v", obj)
		return
	}

	log.Debugf("nodeAdd: node=%+v", node)
	nodeMoID, ok := node.ObjectMeta.Annotations[common.HostMoidAnnotationKey]
	if !ok {
		log.Debugf("nodeAdd: %s annotation not found on the node %s", common.HostMoidAnnotationKey, node.Name)
		return
	}
	k8sOrchestratorInstance.nodeIDToNameMap.add(nodeMoID, node.Name)
}

// nodeUpdate updates an entry into nodeIDToNameMap. The node MoID is retrieved from the
// node annotation vmware-system-esxi-node-moid
func nodeUpdate(oldObject interface{}, newObject interface{}) {
	log := logger.GetLogger(context.Background())
	oldnode, ok := oldObject.(*v1.Node)
	if oldnode == nil || !ok {
		log.Warnf("nodeUpdate: unrecognized object %+v", oldObject)
		return
	}

	newnode, ok := newObject.(*v1.Node)
	if newnode == nil || !ok {
		log.Warnf("nodeUpdate: unrecognized object %+v", newObject)
		return
	}

	_, oldOk := oldnode.ObjectMeta.Annotations[common.HostMoidAnnotationKey]
	newNodeMoID, newOk := newnode.ObjectMeta.Annotations[common.HostMoidAnnotationKey]

	if !oldOk && newOk {
		// If annotation is not found on the old node but found on the new one, add it to the map.
		log.Debugf("Adding nodeMoid %s and node name %s to the map.", newNodeMoID, newnode.Name)
		k8sOrchestratorInstance.nodeIDToNameMap.add(newNodeMoID, newnode.Name)
	}
}

// nodeRemove removes an entry from nodeIDToNameMap. The node MoID is retrieved from the
// node annotation vmware-system-esxi-node-moid
func nodeRemove(obj interface{}) {
	log := logger.GetLogger(context.Background())
	node, ok := obj.(*v1.Node)
	if node == nil || !ok {
		log.Warnf("nodeRemove: unrecognized object %+v", obj)
		return
	}

	log.Debugf("nodeRemove: node=%+v", node)
	nodeMoID, ok := node.ObjectMeta.Annotations[common.HostMoidAnnotationKey]
	if !ok {
		log.Debugf("nodeRemove: %s annotation not found on the node %s", common.HostMoidAnnotationKey, node.Name)
		return
	}
	k8sOrchestratorInstance.nodeIDToNameMap.remove(nodeMoID)
}

// GetNodeIDtoNameMap returns a map containing the nodeID to node name
func (c *K8sOrchestrator) GetNodeIDtoNameMap(ctx context.Context) map[string]string {
	return c.nodeIDToNameMap.items
}

// GetFakeAttachedVolumes returns a map of volumeIDs to a bool, which is set
// to true if volumeID key is fake attached else false
func (c *K8sOrchestrator) GetFakeAttachedVolumes(ctx context.Context, volumeIDs []string) map[string]bool {
	log := logger.GetLogger(ctx)
	volumeIDToFakeAttachedMap := make(map[string]bool)
	for _, volumeID := range volumeIDs {
		// Check pvc annotations.
		pvcAnn, err := c.getPVCAnnotations(ctx, volumeID)
		if err != nil {
			if err.Error() == common.ErrNotFound.Error() {
				// PVC not found, which means PVC could have been deleted. No need to proceed.
				log.Debugf("PVC not found, which means PVC could have been deleted. No need to proceed.")
				continue
			}
			log.Errorf("GetFakeAttachedVolumes: failed to get pvc annotations for volume ID %s "+
				"while checking if it was fake attached", volumeID)
			continue
		}
		val, found := pvcAnn[common.AnnFakeAttached]
		if found && val == "yes" {
			volumeIDToFakeAttachedMap[volumeID] = true
		} else {
			volumeIDToFakeAttachedMap[volumeID] = false
		}
	}
	return volumeIDToFakeAttachedMap
}

// GetVolumeAttachment returns the VA object by using the given volumeId & nodeName
func (c *K8sOrchestrator) GetVolumeAttachment(ctx context.Context, volumeId string, nodeName string) (
	*storagev1.VolumeAttachment, error) {
	log := logger.GetLogger(ctx)
	sha256Res := sha256.Sum256([]byte(fmt.Sprintf("%s%s%s", volumeId, common.VSphereCSIDriverName, nodeName)))
	sha256VaName := fmt.Sprintf("csi-%x", sha256Res)
	volumeAttachment, err := c.k8sClient.StorageV1().VolumeAttachments().Get(ctx, sha256VaName, metav1.GetOptions{})
	if err != nil {
		log.Errorf("failed to get the volumeattachment %q from API server Err: %v", sha256VaName, err)
		return nil, err
	}
	return volumeAttachment, nil
}

// GetAllVolumes returns list of volumes in a bound state for wcp clusters.
// This will not return VCP-CSI migrated volumes.
func (c *K8sOrchestrator) GetAllVolumes() []string {
	volumeIDs := make([]string, 0)
	for volumeID := range c.volumeIDToPvcMap.items {
		volumeIDs = append(volumeIDs, volumeID)
	}
	return volumeIDs
}

// AnnotateVolumeSnapshot annotates the volumesnapshot CR in k8s cluster
func (c *K8sOrchestrator) AnnotateVolumeSnapshot(ctx context.Context, volumeSnapshotName string,
	volumeSnapshotNamespace string, annotations map[string]string) (bool, error) {
	return c.updateVolumeSnapshotAnnotations(ctx, volumeSnapshotName, volumeSnapshotNamespace, annotations)
}

// GetConfigMap checks if ConfigMap with given name exists in the given namespace.
// If it exists, this function returns ConfigMap data, otherwise returns error.
func (c *K8sOrchestrator) GetConfigMap(ctx context.Context, name string, namespace string) (map[string]string, error) {
	log := logger.GetLogger(ctx)
	var err error
	var cm *v1.ConfigMap

	if cm, err = c.k8sClient.CoreV1().ConfigMaps(namespace).Get(ctx, name, metav1.GetOptions{}); err == nil {
		log.Infof("ConfigMap with name %s already exists in namespace %s", name, namespace)
		return cm.Data, nil
	}

	return nil, err
}

// CreateConfigMap creates the ConfigMap with given name, namespace, data and
// immutable parameter values.
func (c *K8sOrchestrator) CreateConfigMap(ctx context.Context, name string, namespace string,
	data map[string]string, isImmutable bool) error {
	log := logger.GetLogger(ctx)

	configMap := v1.ConfigMap{
		TypeMeta: metav1.TypeMeta{
			Kind:       "ConfigMap",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Data:      data,
		Immutable: &isImmutable,
	}

	_, err := c.k8sClient.CoreV1().ConfigMaps(namespace).Create(ctx, &configMap, metav1.CreateOptions{})
	if err != nil {
		return logger.LogNewErrorf(log, "Error occurred while creating the ConfigMap %s in namespace %s, Err: %v",
			name, namespace, err)
	}

	return nil
}

// GetPVNameFromCSIVolumeID retrieves the pv name from volumeID using volumeIDToNameMap.
func (c *K8sOrchestrator) GetPVNameFromCSIVolumeID(volumeID string) (string, bool) {
	return c.volumeIDToNameMap.get(volumeID)
}

// GetPVCNameFromCSIVolumeID returns `pvc name` and `pvc namespace` for the given volumeID using volumeIDToPvcMap.
func (c *K8sOrchestrator) GetPVCNameFromCSIVolumeID(volumeID string) (
	pvcName string, pvcNamespace string, exists bool) {
	namespacedName, ok := c.volumeIDToPvcMap.get(volumeID)
	if !ok {
		return
	}

	parts := strings.Split(namespacedName, "/")
	return parts[1], parts[0], true
}

// GetVolumeIDFromPVCName returns volumeID the given pvcName using pvcToVolumeIDMap.
// PVC name is its namespaced name.
func (c *K8sOrchestrator) GetVolumeIDFromPVCName(pvcName string) (string, bool) {
	return c.pvcToVolumeIDMap.get(pvcName)
}

// IsLinkedCloneRequest checks if the pvc is a linked clone request
func (c *K8sOrchestrator) IsLinkedCloneRequest(ctx context.Context, pvcName string, pvcNamespace string) (bool, error) {
	log := logger.GetLogger(ctx)
	if pvcName == "" || pvcNamespace == "" {
		errMsg := "cannot determine if it's a LinkedClone request. PVC name and/or namespace are missing"
		return false, logger.LogNewErrorf(log, "%s", errMsg)
	}
	pvcObj, err := c.k8sClient.CoreV1().PersistentVolumeClaims(pvcNamespace).Get(ctx, pvcName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.Errorf("PVC %s is not found in namespace %s using informer manager, err: %+v",
				pvcName, pvcNamespace, err)
			return false, common.ErrNotFound
		}
		log.Errorf("failed to get pvc: %s in namespace: %s. err=%v", pvcName, pvcNamespace, err)
		return false, err
	}
	hasLinkedCloneAnn := metav1.HasAnnotation(pvcObj.ObjectMeta, common.AnnKeyLinkedClone)
	isLinkedCloneSupported := c.IsFSSEnabled(ctx, common.LinkedCloneSupport)

	if hasLinkedCloneAnn && !isLinkedCloneSupported {
		log.Errorf("linked clone support is not enabled for the linked clone request pvc %s in namespace %s",
			pvcName, pvcNamespace)
		return false, errors.New("linked clone support is not enabled for the linked clone " +
			"request pvc " + pvcName + " in namespace " + pvcNamespace)
	}
	if hasLinkedCloneAnn {
		return true, nil
	}
	// default false
	return false, nil
}

// GetLinkedCloneVolumeSnapshotSourceUUID retrieves the source of the LinkedClone. For now, it's going to be
// the VolumeSnapshot
func (c *K8sOrchestrator) GetLinkedCloneVolumeSnapshotSourceUUID(ctx context.Context, pvcName string,
	pvcNamespace string) (string, error) {
	log := logger.GetLogger(ctx)
	if pvcName == "" || pvcNamespace == "" {
		errMsg := "cannot retrieve LinkedClone's VolumeSnapshot source. PVC name and/or namespace are missing"
		return "", logger.LogNewErrorf(log, "%s", errMsg)
	}
	linkedClonePVC, err := c.k8sClient.CoreV1().PersistentVolumeClaims(pvcNamespace).Get(ctx, pvcName,
		metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.Errorf("PVC %s is not found in namespace %s, err: %+v",
				pvcName, pvcNamespace, err)
			return "", common.ErrNotFound
		}
		log.Errorf("failed to get pvc: %s in namespace: %s. err=%v", pvcName, pvcNamespace, err)
		return "", err
	}

	// Retrieve the VolumeSnapshot from which the LinkedClone is being created
	dataSource, err := GetPVCDataSource(ctx, linkedClonePVC)
	if err != nil {
		log.Errorf("failed to get data source for linked clone PVC %s in "+
			"namespace %s. err: %v", pvcName, pvcNamespace, err)
		return "", err
	}
	volumeSnapshot, err := c.snapshotterClient.SnapshotV1().VolumeSnapshots(dataSource.Namespace).Get(ctx,
		dataSource.Name, metav1.GetOptions{})
	if err != nil {
		log.Errorf("failed to get source volumesnaphot %s/%s for linked clone PVC %s in "+
			"namespace %s. err: %v", dataSource.Namespace, dataSource.Name, pvcName, pvcNamespace, err)
		return "", err
	}
	vsUID := string(volumeSnapshot.UID)
	log.Debugf("volumesnaphot %s/%s  has UID: %s for linked clone PVC %s/%s ",
		dataSource.Namespace, dataSource.Name, vsUID, pvcName, pvcNamespace)
	return vsUID, nil
}

// PreLinkedCloneCreateAction updates the PVC label with the values specified in map
func (c *K8sOrchestrator) PreLinkedCloneCreateAction(ctx context.Context, pvcName string, pvcNamespace string) error {
	log := logger.GetLogger(ctx)
	if pvcName == "" || pvcNamespace == "" {
		errMsg := "error updating the LinkedClone PVC label as pvc name or namespace is empty"
		return logger.LogNewErrorf(log, "%s", errMsg)
	}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {

		linkedClonePVC, err := c.k8sClient.CoreV1().PersistentVolumeClaims(pvcNamespace).Get(ctx, pvcName,
			metav1.GetOptions{})
		if err != nil {
			log.Errorf("failed to get pvc: %s in namespace: %s. err=%v", pvcName, pvcNamespace, err)
			return err
		}

		if linkedClonePVC.Labels == nil {
			linkedClonePVC.Labels = make(map[string]string)
		}
		// Add label
		if _, ok := linkedClonePVC.Labels[common.AnnKeyLinkedClone]; !ok {
			linkedClonePVC.Labels[common.LinkedClonePVCLabel] = linkedClonePVC.Annotations[common.AttributeIsLinkedClone]
		}

		_, err = c.k8sClient.CoreV1().PersistentVolumeClaims(pvcNamespace).Update(ctx, linkedClonePVC, metav1.UpdateOptions{})
		if err != nil {
			log.Errorf("failed to add linked clone label for PVC %s/%s. Error: %+v, retrying...",
				pvcNamespace, pvcName, err)
			return err
		}
		log.Infof("Successfully added linked clone label for PVC %s/%s",
			pvcNamespace, pvcName)
		return nil
	})
}

// GetVolumeSnapshotPVCSource retrieves the PVC from which the VolumeSnapshot was taken.
func (c *K8sOrchestrator) GetVolumeSnapshotPVCSource(ctx context.Context, volumeSnapshotNamespace string,
	volumeSnapshotName string) (*v1.PersistentVolumeClaim, error) {
	log := logger.GetLogger(ctx)
	if volumeSnapshotNamespace == "" || volumeSnapshotName == "" {
		errMsg := "error getting volume snapshot PVC source as volumesnapshot name and/or namespace is empty"
		return nil, logger.LogNewErrorf(log, "%s", errMsg)
	}
	volumeSnapshot, err := c.snapshotterClient.SnapshotV1().VolumeSnapshots(volumeSnapshotNamespace).Get(
		ctx, volumeSnapshotName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("error getting snapshot %s/%s from API server. Error: %v",
			volumeSnapshotNamespace, volumeSnapshotName, err)
	}
	sourcePVCName := volumeSnapshot.Spec.Source.PersistentVolumeClaimName
	// Retrieve the source volume
	sourcePVC, err := c.k8sClient.CoreV1().PersistentVolumeClaims(volumeSnapshotNamespace).Get(ctx, *sourcePVCName,
		metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("error getting source PVC: %s for snapshot %s/%s from API server. Error: %v",
			*sourcePVCName, volumeSnapshotNamespace, volumeSnapshotName, err)
	}
	log.Infof("GetVolumeSnapshotPVCSource: successfully retrieved source PVC %s for snapshot %s/%s",
		sourcePVC.Name, volumeSnapshotNamespace, volumeSnapshotName)
	return sourcePVC, nil
}

// UpdatePersistentVolumeLabel Updates the PV label with the specified key value.
func (c *K8sOrchestrator) UpdatePersistentVolumeLabel(ctx context.Context,
	pvName string, key string, value string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		log := logger.GetLogger(ctx)
		pv, err := c.k8sClient.CoreV1().PersistentVolumes().Get(ctx, pvName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("error getting PV %s from API server: %w", pvName, err)
		}
		if pv.Labels == nil {
			pv.Labels = make(map[string]string)
		}
		pv.Labels[key] = value
		_, err = c.k8sClient.CoreV1().PersistentVolumes().Update(ctx, pv, metav1.UpdateOptions{})
		if err != nil {
			errMsg := fmt.Sprintf("error updating PV %s with labels %s/%s. Error: %v", pvName, key, value, err)
			log.Error(errMsg)
			return err
		}
		log.Infof("Successfully updated PV %s with label key:%s value:%s", pvName, key, value)
		return nil
	})
}

// GetPVCDataSource Retrieves the VolumeSnapshot source when a PVC from VolumeSnapshot is being created.
func GetPVCDataSource(ctx context.Context, claim *v1.PersistentVolumeClaim) (*v1.ObjectReference, error) {
	var dataSource v1.ObjectReference
	if claim.Spec.DataSourceRef != nil {
		dataSource.Kind = claim.Spec.DataSourceRef.Kind
		dataSource.Name = claim.Spec.DataSourceRef.Name
		if claim.Spec.DataSourceRef.APIGroup != nil {
			dataSource.APIVersion = *claim.Spec.DataSourceRef.APIGroup
		}
		if claim.Spec.DataSourceRef.Namespace != nil {
			dataSource.Namespace = *claim.Spec.DataSourceRef.Namespace
		} else {
			dataSource.Namespace = claim.Namespace
		}
	} else if claim.Spec.DataSource != nil {
		dataSource.Kind = claim.Spec.DataSource.Kind
		dataSource.Name = claim.Spec.DataSource.Name

		if claim.Spec.DataSource.APIGroup != nil {
			dataSource.APIVersion = *claim.Spec.DataSource.APIGroup
		}
		dataSource.Namespace = claim.Namespace
	} else {
		return nil, nil
	}
	return &dataSource, nil
}
