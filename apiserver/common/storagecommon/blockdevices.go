// Copyright 2015 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package storagecommon

import (
	"strings"

	"github.com/juju/loggo"

	"github.com/juju/juju/state"
	"github.com/juju/juju/storage"
)

var logger = loggo.GetLogger("juju.apiserver.storagecommon")

// BlockDeviceFromState translates a state.BlockDeviceInfo to a
// storage.BlockDevice.
func BlockDeviceFromState(in state.BlockDeviceInfo) storage.BlockDevice {
	return storage.BlockDevice{
		DeviceName:     in.DeviceName,
		DeviceLinks:    in.DeviceLinks,
		Label:          in.Label,
		UUID:           in.UUID,
		HardwareId:     in.HardwareId,
		WWN:            in.WWN,
		BusAddress:     in.BusAddress,
		Size:           in.Size,
		FilesystemType: in.FilesystemType,
		InUse:          in.InUse,
		MountPoint:     in.MountPoint,
		SerialId:       in.SerialId,
	}
}

// MatchingBlockDevice finds the block device that matches the
// provided volume info and volume attachment info.
func MatchingBlockDevice(
	blockDevices []state.BlockDeviceInfo,
	volumeInfo state.VolumeInfo,
	attachmentInfo state.VolumeAttachmentInfo,
	planBlockInfo state.BlockDeviceInfo,
) (*state.BlockDeviceInfo, bool) {
	logger.Tracef("looking for block device to match one of planBlockInfo %#v volumeInfo %#v attachmentInfo %#v",
		planBlockInfo, volumeInfo, attachmentInfo)

	if planBlockInfo.HardwareId != "" {
		for _, dev := range blockDevices {
			if planBlockInfo.HardwareId == dev.HardwareId {
				logger.Tracef("plan hwid match on %v", planBlockInfo.HardwareId)
				return &dev, true
			}
		}
		logger.Tracef("no match for block device hardware id: %v", planBlockInfo.HardwareId)
	}

	if planBlockInfo.WWN != "" {
		for _, dev := range blockDevices {
			if planBlockInfo.WWN == dev.WWN {
				logger.Tracef("plan wwn match on %v", planBlockInfo.WWN)
				return &dev, true
			}
		}
		logger.Tracef("no match for block device wwn: %v", planBlockInfo.WWN)
	}

	if planBlockInfo.DeviceName != "" {
		for _, dev := range blockDevices {
			if planBlockInfo.DeviceName == dev.DeviceName {
				logger.Tracef("plan device name match on %v", planBlockInfo.DeviceName)
				return &dev, true
			}
		}
		logger.Tracef("no match for block device name: %v", planBlockInfo.DeviceName)
	}

	if volumeInfo.WWN != "" {
		for _, dev := range blockDevices {
			if volumeInfo.WWN == dev.WWN {
				logger.Tracef("wwn match on %v", volumeInfo.WWN)
				return &dev, true
			}
		}
		logger.Tracef("no match for block device wwn: %v", volumeInfo.WWN)
	}

	if volumeInfo.HardwareId != "" {
		for _, dev := range blockDevices {
			if volumeInfo.HardwareId == dev.HardwareId {
				logger.Tracef("hwid match on %v", volumeInfo.HardwareId)
				return &dev, true
			}
		}
		logger.Tracef("no match for block device hardware id: %v", volumeInfo.HardwareId)
	}

	if volumeInfo.VolumeId != "" {
		for _, dev := range blockDevices {
			if dev.SerialId != "" && strings.HasPrefix(volumeInfo.VolumeId, dev.SerialId) {
				logger.Tracef("serial id %v match on volume id %v", dev.SerialId, volumeInfo.VolumeId)
				return &dev, true
			}
		}
		logger.Tracef("no match for block device volume id: %v", volumeInfo.VolumeId)
	}

	if attachmentInfo.BusAddress != "" {
		for _, dev := range blockDevices {
			if attachmentInfo.BusAddress == dev.BusAddress {
				logger.Tracef("bus address match on %v", attachmentInfo.BusAddress)
				return &dev, true
			}
		}
		logger.Tracef("no match for block device bus address: %v", attachmentInfo.BusAddress)
	}

	if attachmentInfo.DeviceLink != "" {
		for _, dev := range blockDevices {
			for _, link := range dev.DeviceLinks {
				if attachmentInfo.DeviceLink == link {
					logger.Tracef("device link match on %v", attachmentInfo.DeviceLink)
					return &dev, true
				}
			}
		}
		logger.Tracef("no match for block device dev link: %v", attachmentInfo.DeviceLink)
	}

	if attachmentInfo.DeviceName != "" {
		for _, dev := range blockDevices {
			if attachmentInfo.DeviceName == dev.DeviceName {
				logger.Tracef("device name match on %v", attachmentInfo.DeviceName)
				return &dev, true
			}
		}
		logger.Tracef("no match for block device name: %v", attachmentInfo.DeviceName)
	}
	return nil, false
}
