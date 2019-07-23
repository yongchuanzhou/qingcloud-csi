/*
Copyright (C) 2018 Yunify, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this work except in compliance with the License.
You may obtain a copy of the License in the LICENSE file, or at:

http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package rpcserver

import (
	"fmt"
	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/golang/protobuf/ptypes"
	"github.com/golang/protobuf/ptypes/timestamp"
	"github.com/yunify/qingcloud-csi/pkg/cloud"
	"github.com/yunify/qingcloud-csi/pkg/common"
	"github.com/yunify/qingcloud-csi/pkg/disk/driver"
	"github.com/yunify/qingcloud-sdk-go/service"
	"golang.org/x/net/context"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog"
	"reflect"
	"strings"
	"time"
)

type ControllerServer struct {
	driver *driver.DiskDriver
	cloud  cloud.CloudManager
	// TODO: add mutex
}

// NewControllerServer
// Create controller server
func NewControllerServer(d *driver.DiskDriver, c cloud.CloudManager) *ControllerServer {
	return &ControllerServer{
		driver: d,
		cloud:  c,
	}
}

// This operation MUST be idempotent
// This operation MAY create three types of volumes:
// 1. Empty volumes: CREATE_DELETE_VOLUME
// 2. Restore volume from snapshot: CREATE_DELETE_VOLUME and CREATE_DELETE_SNAPSHOT
// 3. Clone volume: CREATE_DELETE_VOLUME and CLONE_VOLUME
// csi.CreateVolumeRequest: name 				+Required
//							capability			+Required
func (cs *ControllerServer) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse,
	error) {
	funcName := "CreateVolume"
	info, hash := common.EntryFunction(funcName)
	klog.Info(info)
	defer klog.Info(common.ExitFunction(funcName, hash))
	// 0. Prepare
	if isValid := cs.driver.ValidateControllerServiceRequest(csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME); isValid != true {
		return nil, status.Error(codes.Unimplemented, "unsupported controller server capability")
	}
	// Required volume capability
	if req.GetVolumeCapabilities() == nil {
		return nil, status.Error(codes.InvalidArgument, "volume capabilities missing in request")
	} else if !cs.driver.ValidateVolumeCapabilities(req.GetVolumeCapabilities()) {
		return nil, status.Error(codes.InvalidArgument, "volume capabilities not match")
	}
	// Check sanity of request Name, Volume Capabilities
	if len(req.GetName()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "volume name missing in request")
	}
	volName := req.GetName()
	// Pick one topology
	var top *driver.Topology
	if req.GetAccessibilityRequirements() != nil && cs.driver.ValidatePluginCapabilityService(csi.
		PluginCapability_Service_VOLUME_ACCESSIBILITY_CONSTRAINTS) {
		klog.Info("Pick topology from CreateVolumeRequest.AccessibilityRequirements")
		var err error
		top, err = cs.PickTopology(req.GetAccessibilityRequirements())
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
	} else {
		klog.Info("Set zone in topology")
		top = driver.NewTopology(cs.cloud.GetZone(), 0)
	}
	klog.Infof("Picked topology is %v", top)
	// create StorageClass object
	sc, err := driver.NewQingStorageClassFromMap(req.GetParameters())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	klog.Infof("Create storage class %v", sc)
	// get request volume capacity range
	requiredSizeByte, err := sc.GetRequiredVolumeSizeByte(req.GetCapacityRange())
	if err != nil {
		return nil, status.Errorf(codes.OutOfRange, "unsupported capacity range, error: %s", err.Error())
	}
	klog.Infof("Get required creating volume size in bytes %d", requiredSizeByte)

	// should not fail when requesting to create a volume with already existing name and same capacity
	// should fail when requesting to create a volume with already existing name and different capacity.
	exVol, err := cs.cloud.FindVolumeByName(volName)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "find volume by name error: %s, %s", volName,
			err.Error())
	}
	if exVol != nil {
		klog.Infof("%s: Request volume name: %s, request size %d bytes, type: %d, zone: %s", hash, volName,
			requiredSizeByte, sc.DiskType, top.GetZone())
		klog.Infof("%s: Exist volume name: %s, id: %s, capacity: %d bytes, type: %d, zone: %s",
			hash, *exVol.VolumeName, *exVol.VolumeID, common.GibToByte(*exVol.Size), *exVol.VolumeType, top.GetZone())
		exVolSizeByte := common.GibToByte(*exVol.Size)
		if common.IsValidCapacityBytes(exVolSizeByte, req.GetCapacityRange()) &&
			*exVol.VolumeType == sc.DiskType.Int() &&
			cs.IsValidTopology(exVol, req.GetAccessibilityRequirements()) {
			// existing volume is compatible with new request and should be reused.
			klog.Infof("Volume %s already exists and compatible with %s", volName, *exVol.VolumeID)
			return &csi.CreateVolumeResponse{
				Volume: &csi.Volume{
					VolumeId:           *exVol.VolumeID,
					CapacityBytes:      exVolSizeByte,
					VolumeContext:      req.GetParameters(),
					AccessibleTopology: cs.GetVolumeTopology(exVol),
				},
			}, nil
		} else {
			klog.Errorf("volume %s/%s already exist but is incompatible", volName, *exVol.VolumeID)
			return nil, status.Errorf(codes.AlreadyExists, "volume %s already exist but is incompatible", volName)
		}
	}

	// do create volume
	volContSrc := req.GetVolumeContentSource()
	if volContSrc == nil {
		// create an empty volume
		klog.Infof("Create an empty volume")
		requiredSizeGib := common.ByteCeilToGib(requiredSizeByte)
		klog.Infof("%s: Creating empty volume %s with %d Gib in zone %s...", hash, volName, requiredSizeGib,
			top.GetZone())
		newVolId, err := cs.cloud.CreateVolume(volName, requiredSizeGib, sc.Replica, sc.DiskType.Int(), top.GetZone())
		if err != nil {
			klog.Errorf("Failed to create volume %s, error: %v", volName, err)
			return nil, status.Error(codes.Internal, err.Error())
		}
		newVolInfo, err := cs.cloud.FindVolume(newVolId)
		if err != nil {
			klog.Errorf("Failed to find volume %s, error: %v", newVolId, err)
			return nil, status.Error(codes.Internal, err.Error())
		}
		if newVolInfo == nil {
			klog.Infof("Cannot find just created volume [%s/%s], please retrying later.", volName, newVolId)
			return nil, status.Errorf(codes.Aborted, "cannot find volume %s", newVolId)
		}

		klog.Infof("%s: Succeed to create empty volume [%s/%s].", hash, volName, newVolId)
		return &csi.CreateVolumeResponse{
			Volume: &csi.Volume{
				VolumeId:           newVolId,
				CapacityBytes:      requiredSizeByte,
				VolumeContext:      req.GetParameters(),
				AccessibleTopology: cs.GetVolumeTopology(newVolInfo),
			},
		}, nil
	} else {
		if volContSrc.GetSnapshot() != nil {
			// Create volume from snapshot
			// Get capability
			if isValid := cs.driver.ValidateControllerServiceRequest(csi.
				ControllerServiceCapability_RPC_CREATE_DELETE_SNAPSHOT); isValid != true {
				return nil, status.Error(codes.Unimplemented, "unsupported controller server capability")
			}
			// Get snapshot id
			if len(volContSrc.GetSnapshot().GetSnapshotId()) == 0 {
				return nil, status.Error(codes.InvalidArgument, "snapshot id missing in request")
			}
			snapId := volContSrc.GetSnapshot().GetSnapshotId()

			// Find snapshot before restore volume from snapshot
			snapInfo, err := cs.cloud.FindSnapshot(snapId)
			if err != nil {
				return nil, status.Error(codes.Internal, err.Error())
			}
			if snapInfo == nil {
				return nil, status.Errorf(codes.NotFound, "cannot find content source snapshot id [%s]", snapId)
			}
			// Compare snapshot required volume size
			requiredRestoreVolumeSizeInBytes := int64(*snapInfo.SnapshotResource.Size) * common.Gib
			if !common.IsValidCapacityBytes(requiredRestoreVolumeSizeInBytes, req.GetCapacityRange()) {
				klog.Errorf("Restore volume request size [%d], out of the capacity range",
					requiredRestoreVolumeSizeInBytes)
				return nil, status.Error(codes.OutOfRange, "unsupported capacity range")
			}
			// restore volume from snapshot
			klog.Infof("%s: Restore volume name [%s] from snapshot id [%s] in zone [%s].", hash, volName,
				snapId, top.GetZone())
			newVolId, err := cs.cloud.CreateVolumeFromSnapshot(volName, snapId, top.GetZone())
			if err != nil {
				return nil, status.Error(codes.Internal, err.Error())
			}
			// Find volume
			newVolInfo, err := cs.cloud.FindVolume(newVolId)
			if err != nil {
				return nil, status.Error(codes.Internal, err.Error())
			}
			if newVolInfo == nil {
				klog.Infof("Cannot find just restore volume [%s/%s], please retrying later.", volName, newVolId)
				return nil, status.Errorf(codes.Aborted, "cannot find volume %s", newVolId)
			}
			actualRestoreVolumeSizeInBytes := int64(*exVol.Size) * common.Gib
			klog.Infof("Get actual restore volume size %d bytes", actualRestoreVolumeSizeInBytes)
			if actualRestoreVolumeSizeInBytes != requiredRestoreVolumeSizeInBytes {
				klog.Errorf("Actual restore volume size %d is not equal to required size %d",
					actualRestoreVolumeSizeInBytes, requiredRestoreVolumeSizeInBytes)
				return nil, status.Errorf(codes.Internal,
					"expected volume size [%d], but actually [%d], volume id [%s], snapshot id [%s]",
					requiredRestoreVolumeSizeInBytes, actualRestoreVolumeSizeInBytes, newVolId, snapId)
			}
			return &csi.CreateVolumeResponse{
				Volume: &csi.Volume{
					VolumeId:           newVolId,
					CapacityBytes:      actualRestoreVolumeSizeInBytes,
					VolumeContext:      req.GetParameters(),
					AccessibleTopology: cs.GetVolumeTopology(newVolInfo),
				},
			}, nil
		} else if volContSrc.GetVolume() != nil {
			// clone volume
			// Get capability
			if isValid := cs.driver.ValidateControllerServiceRequest(csi.
				ControllerServiceCapability_RPC_CLONE_VOLUME); isValid != true {
				klog.Errorf("%s: Invalid create volume req: %v", hash, req)
				return nil, status.Error(codes.Unimplemented, "Unsupported controller server capability")
			}
		}
	}
	return nil, status.Error(codes.Internal, "The plugin SHOULD NOT run here, "+
		"please report at https://github.com/yunify/qingcloud-csi.")
}

// This operation MUST be idempotent
// volume id is REQUIRED in csi.DeleteVolumeRequest
func (cs *ControllerServer) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	klog.Info("----- Start DeleteVolume -----")
	defer klog.Info("===== End DeleteVolume =====")
	if isValid := cs.driver.ValidateControllerServiceRequest(csi.
		ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME); isValid != true {
		klog.Errorf("invalid delete volume req: %v", req)
		return nil, status.Error(codes.Unimplemented, "")
	}
	// Check sanity of request Name, Volume Capabilities
	if len(req.GetVolumeId()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume id missing in request")
	}
	// For now the image get unconditionally deleted, but here retention policy can be checked
	volumeId := req.GetVolumeId()

	// Deleting disk
	klog.Infof("deleting volume %s", volumeId)

	// For idempotent:
	// MUST reply OK when volume does not exist
	volInfo, err := cs.cloud.FindVolume(volumeId)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if volInfo == nil {
		return &csi.DeleteVolumeResponse{}, nil
	}
	// Is volume in use
	if *volInfo.Status == cloud.DiskStatusInuse {
		return nil, status.Errorf(codes.FailedPrecondition, "volume is in use by another resource")
	}
	// Do delete volume
	klog.Infof("Deleting volume %s status %s in zone %s...", volumeId, *volInfo.Status, cs.cloud.GetZone())
	// When return with retry message at deleting volume, retry after several seconds.
	// Retry times is 10.
	// Retry interval is changed from 1 second to 10 seconds.
	for i := 1; i <= 10; i++ {
		err = cs.cloud.DeleteVolume(volumeId)
		if err != nil {
			klog.Infof("Failed to delete disk volume: %s in %s with error: %v", volumeId, cs.cloud.GetZone(), err)
			if strings.Contains(err.Error(), cloud.RetryString) {
				time.Sleep(time.Duration(i*2) * time.Second)
			} else {
				return nil, status.Error(codes.Internal, err.Error())
			}
		} else {
			return &csi.DeleteVolumeResponse{}, nil
		}
	}
	return nil, status.Error(codes.Internal, "Exceed retry times: "+err.Error())
}

// csi.ControllerPublishVolumeRequest: 	volume id 			+ Required
//										node id				+ Required
//										volume capability 	+ Required
//										readonly			+ Required (This field is NOT provided when requesting in Kubernetes)
func (cs *ControllerServer) ControllerPublishVolume(ctx context.Context, req *csi.ControllerPublishVolumeRequest) (*csi.
	ControllerPublishVolumeResponse, error) {
	klog.Info("----- Start ControllerPublishVolume -----")
	defer klog.Info("===== End ControllerPublishVolume =====")
	if isValid := cs.driver.ValidateControllerServiceRequest(csi.
		ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME); isValid != true {
		klog.Errorf("invalid delete volume req: %v", req)
		return nil, status.Error(codes.Unimplemented, "")
	}
	// 0. Preflight
	// check volume id arguments
	if len(req.GetVolumeId()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}
	// check nodeId arguments
	if len(req.GetNodeId()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Node ID missing in request")
	}
	// check volume capability
	if req.GetVolumeCapability() == nil {
		return nil, status.Error(codes.InvalidArgument, "No volume capability is provided ")
	}

	// if volume id not exist
	volumeId := req.GetVolumeId()
	exVol, err := cs.cloud.FindVolume(volumeId)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if exVol == nil {
		return nil, status.Errorf(codes.NotFound, "Volume: %s does not exist", volumeId)
	}

	// if instance id not exist
	nodeId := req.GetNodeId()
	exIns, err := cs.cloud.FindInstance(nodeId)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if exIns == nil {
		return nil, status.Errorf(codes.NotFound, "Node: %s does not exist", nodeId)
	}

	// Volume published to another node
	if len(*exVol.Instance.InstanceID) != 0 {
		if *exVol.Instance.InstanceID == nodeId {
			klog.Warningf("volume %s has been already attached on instance %s", volumeId, nodeId)
			return &csi.ControllerPublishVolumeResponse{}, nil
		} else {
			klog.Warningf("volume %s expected attached on instance %s, but actually %s", volumeId, nodeId,
				*exVol.Instance.InstanceID)
			return nil, status.Error(codes.FailedPrecondition, "Volume published to another node")
		}
	}

	if req.GetVolumeCapability() == nil {
		return nil, status.Error(codes.InvalidArgument, "Volume capability missing in request")
	}
	// 1. Attach
	// attach volume
	klog.Infof("Attaching volume %s to instance %s in zone %s...", volumeId, nodeId, cs.cloud.GetZone())
	err = cs.cloud.AttachVolume(volumeId, nodeId)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	// When return with retry message at describe volume, retry after several seconds.
	// Retry times is 3.
	// Retry interval is changed from 1 second to 3 seconds.
	for i := 1; i <= 3; i++ {
		volInfo, err := cs.cloud.FindVolume(volumeId)
		if err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
		// check device path
		if *volInfo.Instance.Device != "" {
			// found device path
			klog.Infof("Attaching volume %s on instance %s succeed.", volumeId, nodeId)
			return &csi.ControllerPublishVolumeResponse{}, nil
		} else {
			// cannot found device path
			klog.Infof("Cannot find device path and retry to find volume device %s", volumeId)
			time.Sleep(time.Duration(i) * time.Second)
		}
	}
	// Cannot find device path
	// Try to detach volume
	klog.Infof("Cannot find device path and going to detach volume %s", volumeId)
	if err := cs.cloud.DetachVolume(volumeId, nodeId); err != nil {
		return nil, status.Errorf(codes.Internal,
			"cannot find device path, detach volume %s failed", volumeId)
	} else {
		return nil, status.Errorf(codes.Internal,
			"cannot find device path, volume %s has been detached, please try attaching to instance %s again.",
			volumeId, nodeId)
	}
}

// This operation MUST be idempotent
// csi.ControllerUnpublishVolumeRequest: 	volume id	+Required
func (cs *ControllerServer) ControllerUnpublishVolume(ctx context.Context, req *csi.ControllerUnpublishVolumeRequest) (*csi.
	ControllerUnpublishVolumeResponse, error) {
	klog.Info("----- Start ControllerUnpublishVolume -----")
	defer klog.Info("===== End ControllerUnpublishVolume =====")
	if isValid := cs.driver.ValidateControllerServiceRequest(csi.
		ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME); isValid != true {
		klog.Errorf("invalid unpublish volume req: %v", req)
		return nil, status.Error(codes.Unimplemented, "")
	}
	// 0. Preflight
	// check arguments
	if len(req.GetVolumeId()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}
	volumeId := req.GetVolumeId()
	nodeId := req.GetNodeId()

	// 1. Detach
	// check volume exist
	exVol, err := cs.cloud.FindVolume(volumeId)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if exVol == nil {
		return nil, status.Errorf(codes.NotFound, "Volume: %s does not exist", volumeId)
	} else if exVol.Instance == nil || *exVol.Instance.InstanceID == "" {
		klog.Warningf("Volume %s is not attached to any instance", volumeId)
		return &csi.ControllerUnpublishVolumeResponse{}, nil
	}

	// check node exist
	exIns, err := cs.cloud.FindInstance(nodeId)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if exIns == nil {
		return nil, status.Errorf(codes.NotFound, "Node: %s does not exist", nodeId)
	}

	// do detach
	klog.Infof("Detaching volume %s to instance %s in zone %s...", volumeId, nodeId, cs.cloud.GetZone())
	err = cs.cloud.DetachVolume(volumeId, nodeId)
	if err != nil {
		klog.Errorf("Failed to detach disk image: %s from instance %s with error: %v",
			volumeId, nodeId, err)
		return nil, err
	}

	return &csi.ControllerUnpublishVolumeResponse{}, nil
}

// This operation MUST be idempotent
// csi.ValidateVolumeCapabilitiesRequest: 	volume id 			+ Required
// 											volume capability 	+ Required
func (cs *ControllerServer) ValidateVolumeCapabilities(ctx context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.
	ValidateVolumeCapabilitiesResponse, error) {
	klog.Info("----- Start ValidateVolumeCapabilities -----")
	defer klog.Info("===== End ValidateVolumeCapabilities =====")

	// require volume id parameter
	if len(req.GetVolumeId()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "No volume id is provided")
	}

	// require capability parameter
	if len(req.GetVolumeCapabilities()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "No volume capabilities are provided")
	}

	// check volume exist
	volumeId := req.GetVolumeId()
	vol, err := cs.cloud.FindVolume(volumeId)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if vol == nil {
		return nil, status.Errorf(codes.NotFound, "volume %s does not exist", volumeId)
	}

	// check capability
	for _, c := range req.GetVolumeCapabilities() {
		found := false
		for _, c1 := range cs.driver.GetVolumeCapability() {
			if c1.GetMode() == c.GetAccessMode().GetMode() {
				found = true
			}
		}
		if !found {
			return &csi.ValidateVolumeCapabilitiesResponse{
				Message: "Driver doesnot support mode:" + c.GetAccessMode().GetMode().String(),
			}, status.Error(codes.InvalidArgument, "Driver doesnot support mode:"+c.GetAccessMode().GetMode().String())
		}
	}
	return &csi.ValidateVolumeCapabilitiesResponse{}, nil
}

// ControllerExpandVolume allows the CO to expand the size of a volume
// volume id is REQUIRED in csi.ControllerExpandVolumeRequest
// capacity range is REQUIRED in csi.ControllerExpandVolumeRequest
func (cs *ControllerServer) ControllerExpandVolume(ctx context.Context, req *csi.ControllerExpandVolumeRequest,
) (*csi.ControllerExpandVolumeResponse, error) {
	functionName := "ControllerExpandVolume"
	info, hash := common.EntryFunction(functionName)
	defer klog.Info(common.ExitFunction(functionName, hash))
	klog.Info(info)
	// 0. check input args
	// require volume id parameter
	if len(req.GetVolumeId()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "No volume id is provided")
	}

	// 1. Check volume status
	// does volume exist
	volumeId := req.GetVolumeId()
	volInfo, err := cs.cloud.FindVolume(volumeId)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if volInfo == nil {
		return nil, status.Errorf(codes.NotFound, "Volume: %s does not exist", volumeId)
	}

	// volume in use
	if *volInfo.Status == cloud.DiskStatusInuse {
		return nil, status.Errorf(codes.FailedPrecondition,
			"volume %s currently published on a node but plugin only support OFFLINE expansion", volumeId)
	}

	// 2. Get capacity
	volType := driver.VolumeType(*volInfo.VolumeType)
	if !volType.IsValid() {
		klog.Errorf("%s: Unsupported volume [%s] type [%d]", hash, volumeId, *volInfo.VolumeType)
		return nil, status.Errorf(codes.Internal, "Unsupported volume [%s] type [%d]", volumeId, *volInfo.VolumeType)
	} else {
		klog.Infof("%s: Succeed to get volume [%s] type [%s]", hash, volumeId, driver.VolumeTypeName[volType])
	}

	sc := driver.NewDefaultQingStorageClassFromType(volType)
	requiredSizeBytes, err := sc.GetRequiredVolumeSizeByte(req.GetCapacityRange())
	if err != nil {
		return nil, status.Errorf(codes.OutOfRange, err.Error())
	}

	// 3. Expand volume
	if requiredSizeBytes%common.Gib != 0 {
		return nil, status.Errorf(codes.OutOfRange, "required size bytes %d cannot be divided into Gib %d",
			requiredSizeBytes, common.Gib)
	}
	requiredSizeGib := int(requiredSizeBytes / common.Gib)
	err = cs.cloud.ResizeVolume(volumeId, requiredSizeGib)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &csi.ControllerExpandVolumeResponse{
		CapacityBytes:         requiredSizeBytes,
		NodeExpansionRequired: true,
	}, nil
}

func (cs *ControllerServer) ListVolumes(ctx context.Context, req *csi.ListVolumesRequest) (*csi.ListVolumesResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (cs *ControllerServer) GetCapacity(ctx context.Context, req *csi.GetCapacityRequest) (*csi.GetCapacityResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

// CreateSnapshot allows the CO to create a snapshot.
// This operation MUST be idempotent.
// 1. If snapshot successfully cut and ready to use, the plugin MUST reply 0 OK.
// 2. If an error occurs before a snapshot is cut, the plugin SHOULD reply a corresponding error code.
// 3. If snapshot successfully cut but still being precessed,
// the plugin SHOULD return 0 OK and ready_to_use SHOULD be set to false.
// Source volume id is REQUIRED
// Snapshot name is REQUIRED
func (cs *ControllerServer) CreateSnapshot(ctx context.Context, req *csi.CreateSnapshotRequest) (*csi.CreateSnapshotResponse,
	error) {
	klog.Info("----- Start CreateSnapshot -----")
	defer klog.Info("===== End CreateSnapshot =====")
	if isValid := cs.driver.ValidateControllerServiceRequest(csi.
		ControllerServiceCapability_RPC_CREATE_DELETE_SNAPSHOT); isValid != true {
		klog.Errorf("invalid create snapshot request: %v", req)
		return nil, status.Error(codes.Unimplemented, "")
	}
	// 0. Preflight
	// Check source volume id
	klog.Info("Check required parameters")
	if len(req.GetSourceVolumeId()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "volume ID missing in request")
	}
	// Check snapshot name
	if len(req.GetName()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "snapshot name missing in request")
	}

	// Create snapshot manager object
	srcVolId := req.GetSourceVolumeId()
	snapName := req.GetName()
	var ts *timestamp.Timestamp
	var isReadyToUse bool
	// For idempotent
	// If a snapshot corresponding to the specified snapshot name is successfully cut and ready to use (meaning it MAY
	// be specified as a volume_content_source in a CreateVolumeRequest), the Plugin MUST reply 0 OK with the
	// corresponding CreateSnapshotResponse.
	klog.Infof("Find existing snapshot name [%s]", snapName)
	exSnap, err := cs.cloud.FindSnapshotByName(snapName)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "find snapshot by name error: %s, %s", snapName, err.Error())
	}
	if exSnap != nil {
		klog.Infof("Found existing snapshot name [%s], snapshot id [%s], source volume id %s",
			*exSnap.SnapshotName, *exSnap.SnapshotID, *exSnap.Resource.ResourceID)
		if exSnap.Resource != nil && *exSnap.Resource.ResourceType == "volume" &&
			*exSnap.Resource.ResourceID == srcVolId {
			ts, err = ptypes.TimestampProto(*exSnap.CreateTime)
			if err != nil {
				return nil, status.Error(codes.Internal, err.Error())
			}
			if *exSnap.Status == cloud.SnapshotStatusAvailable {
				isReadyToUse = true
			} else {
				isReadyToUse = false
			}
			return &csi.CreateSnapshotResponse{
				Snapshot: &csi.Snapshot{
					SizeBytes:      int64(*exSnap.Size) * common.Mib,
					SnapshotId:     *exSnap.SnapshotID,
					SourceVolumeId: *exSnap.Resource.ResourceID,
					CreationTime:   ts,
					ReadyToUse:     isReadyToUse,
				},
			}, nil
		}
		return nil, status.Errorf(codes.AlreadyExists,
			"snapshot name [%s] id [%s] already exists, but is incompatible with the source volume id [%s]",
			snapName, *exSnap.SnapshotID, srcVolId)
	}
	// Create a new full snapshot
	klog.Infof("Creating snapshot [%s] from volume [%s] in zone [%s]...", snapName, srcVolId, cs.cloud.GetZone())
	snapId, err := cs.cloud.CreateSnapshot(snapName, srcVolId)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "create snapshot [%s] from source volume [%s] error: %s",
			snapName, srcVolId, err.Error())
	}
	klog.Infof("Create snapshot [%s] finished, get snapshot id [%s]", snapName, snapId)
	klog.Infof("Get snapshot id [%s] info...", snapId)
	snapInfo, err := cs.cloud.FindSnapshot(snapId)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Find snapshot [%s] error: %s", snapId, err.Error())
	}
	if snapInfo == nil {
		return nil, status.Errorf(codes.Internal, "cannot find just created snapshot id [%s]", snapId)
	}
	klog.Infof("Succeed to find snapshot id [%s]", snapId)
	ts, err = ptypes.TimestampProto(*snapInfo.CreateTime)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if *snapInfo.Status == cloud.SnapshotStatusAvailable {
		isReadyToUse = true
	} else {
		isReadyToUse = false
	}
	return &csi.CreateSnapshotResponse{
		Snapshot: &csi.Snapshot{
			SizeBytes:      int64(*snapInfo.Size) * common.Mib,
			SnapshotId:     *snapInfo.SnapshotID,
			SourceVolumeId: *snapInfo.Resource.ResourceID,
			CreationTime:   ts,
			ReadyToUse:     isReadyToUse,
		},
	}, nil
}

// CreateSnapshot allows the CO to delete a snapshot.
// This operation MUST be idempotent.
// Snapshot id is REQUIRED
func (cs *ControllerServer) DeleteSnapshot(ctx context.Context, req *csi.DeleteSnapshotRequest) (*csi.DeleteSnapshotResponse,
	error) {
	klog.Info("----- Start DeleteSnapshot -----")
	defer klog.Info("===== End DeleteSnapshot =====")
	if isValid := cs.driver.ValidateControllerServiceRequest(csi.
		ControllerServiceCapability_RPC_CREATE_DELETE_SNAPSHOT); isValid != true {
		klog.Errorf("invalid create snapshot request: %v", req)
		return nil, status.Error(codes.Unimplemented, "")
	}
	// 0. Preflight
	// Check snapshot id
	klog.Info("Check required parameters")
	if len(req.GetSnapshotId()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "snapshot ID missing in request")
	}
	snapId := req.GetSnapshotId()
	// 1. For idempotent:
	// MUST reply OK when snapshot does not exist
	klog.Infof("Find existing snapshot id [%s].", snapId)
	exSnap, err := cs.cloud.FindSnapshot(snapId)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if exSnap == nil {
		klog.Infof("Cannot find snapshot id [%s].", snapId)
		return &csi.DeleteSnapshotResponse{}, nil
	}
	// 2. Delete snapshot
	klog.Infof("Deleting snapshot id [%s] in zone [%s]...", snapId, cs.cloud.GetZone())
	// When return with retry message at deleting snapshot, retry after several seconds.
	// Retry times is 10.
	// Retry interval is changed from 1 second to 10 seconds.
	for i := 1; i <= 10; i++ {
		klog.Infof("Try to delete snapshot id [%s] in [%d] time(s)", snapId, i)
		err = cs.cloud.DeleteSnapshot(snapId)
		if err != nil {
			klog.Infof("Failed to delete snapshot id [%s] in zone [%s] with error: %v", snapId, cs.cloud.GetZone(), err)
			if strings.Contains(err.Error(), cloud.RetryString) {
				sleepTime := time.Duration(i*2) * time.Second
				klog.Infof("Retry to delete snapshot id [%s] after [%f] second(s).", snapId, sleepTime.Seconds())
				time.Sleep(sleepTime)
			} else {
				return nil, status.Error(codes.Internal, err.Error())
			}
		} else {
			klog.Infof("Succeed to delete snapshot id [%s] after [%d] time(s).", snapId, i)
			return &csi.DeleteSnapshotResponse{}, nil
		}
	}
	return nil, status.Error(codes.Internal, "Exceed retry times: "+err.Error())
}

func (cs *ControllerServer) ListSnapshots(ctx context.Context, req *csi.ListSnapshotsRequest) (*csi.ListSnapshotsResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

func (cs *ControllerServer) ControllerGetCapabilities(ctx context.Context,
	req *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	return &csi.ControllerGetCapabilitiesResponse{
		Capabilities: cs.driver.GetControllerCapability(),
	}, nil
}

// pickAvailabilityZone selects 1 zone given topology requirement.
// if not found, empty string is returned.
func (cs *ControllerServer) PickTopology(requirement *csi.TopologyRequirement) (*driver.Topology, error) {
	res := &driver.Topology{}
	if requirement == nil {
		return nil, nil
	}
	for _, topology := range requirement.GetPreferred() {
		for k, v := range topology.GetSegments() {
			switch k {
			case cs.driver.GetTopologyZoneKey():
				res.SetZone(v)
			case cs.driver.GetTopologyInstanceTypeKey():
				t, ok := driver.InstanceTypeValue[v]
				if !ok {
					return nil, fmt.Errorf("unsuport instance type %s", v)
				}
				res.SetInstanceType(t)
			default:
				return res, fmt.Errorf("invalid topology key %s", k)
			}
		}
		return res, nil
	}
	for _, topology := range requirement.GetRequisite() {
		for k, v := range topology.GetSegments() {
			switch k {
			case cs.driver.GetTopologyZoneKey():
				res.SetZone(v)
			case cs.driver.GetTopologyInstanceTypeKey():
				t, ok := driver.InstanceTypeValue[v]
				if !ok {
					return nil, fmt.Errorf("unsuport instance type %s", v)
				}
				res.SetInstanceType(t)
			default:
				return res, fmt.Errorf("invalid topology key %s", k)
			}
		}
		return res, nil
	}
	return nil, nil
}

func (cs *ControllerServer) IsValidTopology(volume *service.Volume, requirement *csi.TopologyRequirement) bool {
	if volume == nil {
		return false
	}
	if requirement == nil || len(requirement.GetRequisite()) == 0 {
		return true
	}
	volTops := cs.GetVolumeTopology(volume)
	res := true
	for _, reqTop := range requirement.GetRequisite() {
		for _, volTop := range volTops {
			if reflect.DeepEqual(reqTop, volTop) {
				return true
			} else {
				res = false
			}
		}
	}
	return res
}

// GetVolumeTopology gets csi topology from volume info.
func (cs *ControllerServer) GetVolumeTopology(volume *service.Volume) []*csi.Topology {
	if volume == nil {
		return nil
	}
	volType := driver.VolumeType(*volume.VolumeType)
	if volType.IsValid() == false {
		return nil
	}
	var res []*csi.Topology
	for _, insType := range driver.VolumeTypeAttachConstraint[volType] {
		res = append(res, &csi.Topology{
			Segments: map[string]string{
				cs.driver.GetTopologyInstanceTypeKey(): driver.InstanceTypeName[insType],
				cs.driver.GetTopologyZoneKey():         *volume.ZoneID,
			},
		})
	}
	return res
}
