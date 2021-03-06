package syncer

import (
	"context"

	"k8s.io/client-go/tools/cache"

	cnstypes "github.com/vmware/govmomi/cns/types"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/vsphere-csi-driver/pkg/apis/migration"
	volumes "sigs.k8s.io/vsphere-csi-driver/pkg/common/cns-lib/volume"
	"sigs.k8s.io/vsphere-csi-driver/pkg/csi/service/common"
	"sigs.k8s.io/vsphere-csi-driver/pkg/csi/service/logger"
	"sigs.k8s.io/vsphere-csi-driver/pkg/csi/types"
	csitypes "sigs.k8s.io/vsphere-csi-driver/pkg/csi/types"
)

// getPVsInBoundAvailableOrReleased return PVs in Bound, Available or Released state
func getPVsInBoundAvailableOrReleased(ctx context.Context, metadataSyncer *metadataSyncInformer) ([]*v1.PersistentVolume, error) {
	log := logger.GetLogger(ctx)
	var pvsInDesiredState []*v1.PersistentVolume
	log.Debugf("FullSync: Getting all PVs in Bound, Available or Released state")
	// Get all PVs from kubernetes
	allPVs, err := metadataSyncer.pvLister.List(labels.Everything())
	if err != nil {
		return nil, err
	}
	for _, pv := range allPVs {
		if (pv.Spec.CSI != nil && pv.Spec.CSI.Driver == csitypes.Name) || (metadataSyncer.coCommonInterface.IsFSSEnabled(ctx, common.CSIMigration) && pv.Spec.VsphereVolume != nil &&
			isValidvSphereVolume(ctx, pv.ObjectMeta)) {
			log.Debugf("FullSync: pv %v is in state %v", pv.Name, pv.Status.Phase)
			if pv.Status.Phase == v1.VolumeBound || pv.Status.Phase == v1.VolumeAvailable || pv.Status.Phase == v1.VolumeReleased {
				pvsInDesiredState = append(pvsInDesiredState, pv)
			}
		}
	}
	return pvsInDesiredState, nil
}

// getBoundPVs is a helper function for VolumeHealthStatus feature and returns PVs in Bound state
func getBoundPVs(ctx context.Context, metadataSyncer *metadataSyncInformer) ([]*v1.PersistentVolume, error) {
	log := logger.GetLogger(ctx)
	var boundPVs []*v1.PersistentVolume
	// Get all PVs from kubernetes
	allPVs, err := metadataSyncer.pvLister.List(labels.Everything())
	if err != nil {
		return nil, err
	}
	for _, pv := range allPVs {
		if pv.Spec.CSI != nil && pv.Spec.CSI.Driver == csitypes.Name {
			log.Debugf("getBoundPVs: pv %s with volumeHandle %s is in state %v", pv.Name, pv.Spec.CSI.VolumeHandle, pv.Status.Phase)
			if pv.Status.Phase == v1.VolumeBound {
				boundPVs = append(boundPVs, pv)
			}
		}
	}
	return boundPVs, nil
}

// getInlineMigratedVolumesInfo is a helper function for retrieving  inline PV information from Pods
func getInlineMigratedVolumesInfo(ctx context.Context, metadataSyncer *metadataSyncInformer) (map[string]string, error) {
	log := logger.GetLogger(ctx)
	inlineVolumes := make(map[string]string)
	// Get all Pods from kubernetes
	allPods, err := metadataSyncer.podLister.List(labels.Everything())
	if err != nil {
		log.Errorf("getInlineMigratedVolumesInfo: failed to fetch the list of pods with err: %+v", err)
		return nil, err
	}
	for _, pod := range allPods {
		for _, volume := range pod.Spec.Volumes {
			// Check if migration is ON and volumes if of type vSphereVolume
			if metadataSyncer.coCommonInterface.IsFSSEnabled(ctx, common.CSIMigration) && volume.VsphereVolume != nil {
				volumeHandle, err := volumeMigrationService.GetVolumeID(ctx, migration.VolumeSpec{VolumePath: volume.VsphereVolume.VolumePath, StoragePolicyName: volume.VsphereVolume.StoragePolicyName})
				if err != nil {
					log.Warnf("FullSync: Failed to get VolumeID from volumeMigrationService for volumePath: %s with error %+v", volume.VsphereVolume.VolumePath, err)
					continue
				}
				inlineVolumes[volume.VsphereVolume.VolumePath] = volumeHandle
			}
		}
	}
	return inlineVolumes, nil
}

// IsValidVolume determines if the given volume mounted by a POD is a valid vsphere volume. Returns the pv and pvc object if true.
func IsValidVolume(ctx context.Context, volume v1.Volume, pod *v1.Pod, metadataSyncer *metadataSyncInformer) (bool, *v1.PersistentVolume, *v1.PersistentVolumeClaim) {
	log := logger.GetLogger(ctx)
	pvcName := volume.PersistentVolumeClaim.ClaimName
	// Get pvc attached to pod
	pvc, err := metadataSyncer.pvcLister.PersistentVolumeClaims(pod.Namespace).Get(pvcName)
	if err != nil {
		log.Errorf("Error getting Persistent Volume Claim for volume %s with err: %v", volume.Name, err)
		return false, nil, nil
	}

	// Get pv object attached to pvc
	pv, err := metadataSyncer.pvLister.Get(pvc.Spec.VolumeName)
	if err != nil {
		log.Errorf("Error getting Persistent Volume for PVC %s in volume %s with err: %v", pvc.Name, volume.Name, err)
		return false, nil, nil
	}

	// Verify if pv is vsphere csi volume
	if (pv.Spec.CSI == nil || pv.Spec.CSI.Driver != csitypes.Name) && (metadataSyncer.coCommonInterface.IsFSSEnabled(ctx, common.CSIMigration) && pv.Spec.VsphereVolume == nil) {
		log.Debugf("Pod %s does not have a valid vSphereVolume. Ignoring the pod update", pod.Name)
		return false, nil, nil
	}
	//Verify if pv is vsphere volume and migration flag is disabled
	if pv.Spec.VsphereVolume != nil && !metadataSyncer.coCommonInterface.IsFSSEnabled(ctx, common.CSIMigration) {
		log.Warnf("%s feature switch is disabled. Cannot update vSphere volume metadata %s for the pod %s", common.CSIMigration, pv.Name, pod.Name)
		return false, nil, nil
	}
	return true, pv, pvc
}

// getQueryResults returns list of CnsQueryResult retrieved using
// queryFilter with offset and limit to query volumes using pagination
// if volumeIds is empty, then all volumes from CNS will be retrieved by pagination
func getQueryResults(ctx context.Context, volumeIds []cnstypes.CnsVolumeId, clusterID string, volumeManager volumes.Manager) ([]*cnstypes.CnsQueryResult, error) {
	log := logger.GetLogger(ctx)
	queryFilter := cnstypes.CnsQueryFilter{
		VolumeIds: volumeIds,
		Cursor: &cnstypes.CnsCursor{
			Offset: 0,
			Limit:  queryVolumeLimit,
		},
	}
	if clusterID != "" {
		queryFilter.ContainerClusterIds = []string{clusterID}
	}
	var allQueryResults []*cnstypes.CnsQueryResult
	for {
		log.Debugf("Query volumes with offset: %v and limit: %v", queryFilter.Cursor.Offset, queryFilter.Cursor.Limit)
		queryResult, err := volumeManager.QueryVolume(ctx, queryFilter)
		if err != nil {
			log.Errorf("failed to QueryVolume using filter: %+v", queryFilter)
			return nil, err
		}
		if queryResult == nil {
			log.Info("Observed empty queryResult")
			break
		}
		allQueryResults = append(allQueryResults, queryResult)
		log.Infof("%v more volumes to be queried", queryResult.Cursor.TotalRecords-queryResult.Cursor.Offset)
		if queryResult.Cursor.Offset == queryResult.Cursor.TotalRecords {
			log.Info("Metadata retrieved for all requested volumes")
			break
		}
		queryFilter.Cursor = &queryResult.Cursor
	}
	return allQueryResults, nil
}

// getPVCKey helps to get the PVC name from PVC object
func getPVCKey(ctx context.Context, obj interface{}) (string, error) {
	log := logger.GetLogger(ctx)

	if unknown, ok := obj.(cache.DeletedFinalStateUnknown); ok && unknown.Obj != nil {
		obj = unknown.Obj
	}
	objKey, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		log.Errorf("Failed to get key from object: %v", err)
		return "", err
	}
	log.Infof("getPVCKey: PVC key %s", objKey)
	return objKey, nil
}

// HasMigratedToAnnotationUpdate returns true if the migrated-to annotation is found in the newer object
func HasMigratedToAnnotationUpdate(ctx context.Context, prevAnnotations map[string]string, newAnnotations map[string]string, objectName string) bool {
	log := logger.GetLogger(ctx)
	// Checking if the migrated-to annotation is found in the newer object
	if _, annMigratedToFound := newAnnotations[common.AnnMigratedTo]; annMigratedToFound {
		if _, annMigratedToFound = prevAnnotations[common.AnnMigratedTo]; !annMigratedToFound {
			log.Debugf("Received %v annotation update for %q", common.AnnMigratedTo, objectName)
			return true
		}
	}
	log.Debugf("%v annotation not found for %q", common.AnnMigratedTo, objectName)
	return false
}

// isValidvSphereVolumeClaim returns true if the given PVC metadata of a vSphere Volume (in-tree volume)
// has migrated-to annotation on the PVC
// or if the PVC was provisioned by CSI driver using in-tree storage class
func isValidvSphereVolumeClaim(ctx context.Context, pvcMetadata metav1.ObjectMeta) bool {
	log := logger.GetLogger(ctx)
	// Checking if the migrated-to annotation is found in the PVC metadata
	if annotation, annMigratedToFound := pvcMetadata.Annotations[common.AnnMigratedTo]; annMigratedToFound {
		if annotation == types.Name && pvcMetadata.Annotations[common.AnnStorageProvisioner] == common.InTreePluginName {
			log.Debugf("%v annotation found with value %q for PVC: %q", common.AnnMigratedTo, types.Name, pvcMetadata.Name)
			return true
		}
	} else { // Checking if the PVC was provisioned by CSI
		if pvcMetadata.Annotations[common.AnnStorageProvisioner] == types.Name {
			log.Debugf("%v annotation found with value %q for PVC: %q", common.AnnStorageProvisioner, types.Name, pvcMetadata.Name)
			return true
		}
	}
	return false
}

// isValidvSphereVolume returns true if the given PV metadata of a vSphere Volume (in-tree volume) and
// has migrated-to annotation on the PV
// or if the PV was provisioned by CSI driver using in-tree storage class
func isValidvSphereVolume(ctx context.Context, pvMetadata metav1.ObjectMeta) bool {
	log := logger.GetLogger(ctx)
	// Checking if the migrated-to annotation is found in the PV metadata
	if annotation, annMigratedToFound := pvMetadata.Annotations[common.AnnMigratedTo]; annMigratedToFound {
		if annotation == types.Name && pvMetadata.Annotations[common.AnnDynamicallyProvisioned] == common.InTreePluginName {
			log.Debugf("%v annotation found with value %q for PV: %q", common.AnnMigratedTo, types.Name, pvMetadata.Name)
			return true
		}
	} else {
		if pvMetadata.Annotations[common.AnnDynamicallyProvisioned] == types.Name {
			log.Debugf("%v annotation found with value %q for PV: %q", common.AnnDynamicallyProvisioned, types.Name, pvMetadata.Name)
			return true
		}
	}
	return false
}
