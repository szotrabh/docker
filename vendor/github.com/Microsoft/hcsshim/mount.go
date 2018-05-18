// +build windows

package hcsshim

import (
	"fmt"
	"path/filepath"

	"github.com/Microsoft/hcsshim/schema/v2"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

// MountContainerLayers is a helper for clients to hide all the complexity of layer mounting
// Layer folder are in order: base, [rolayer1..rolayern,] sandbox
// TODO: Extend for LCOW?
//
// v1/v2: Argon WCOW: Returns the mount path on the host as a volume GUID.
// v1:    Xenon WCOW: Done internally in HCS, so no point calling doing anything here.
// v2:    Xenon WCOW: Returns a CombinedLayersV2 structure where ContainerRootPath is a folder
//                    inside the utility VM which is a GUID mapping of the sandbox folder. Each
//                    of the layers are the VSMB locations where the read-only layers are mounted.

// TODO Should this return a string or an object? More efficient as object, but requires more client work to marshall it again.
func MountContainerLayers(layerFolders []string, uvm *UtilityVM) (interface{}, error) {
	logrus.Debugln("hcsshim::MountContainerLayers", layerFolders)

	if uvm == nil {
		if len(layerFolders) < 2 {
			return nil, fmt.Errorf("need at least two layers - base and sandbox")
		}
		id := filepath.Base(layerFolders[len(layerFolders)-1])
		homeDir := filepath.Dir(layerFolders[len(layerFolders)-1])
		di := DriverInfo{HomeDir: homeDir}

		logrus.Debugln("hcsshim::MountContainerLayers ActivateLayer", di, id)
		if err := ActivateLayer(di, id); err != nil {
			return nil, err
		}
		logrus.Debugln("hcsshim::MountContainerLayers Preparelayer", di, id, layerFolders[:len(layerFolders)-1])
		if err := PrepareLayer(di, id, layerFolders[:len(layerFolders)-1]); err != nil {
			if err2 := DeactivateLayer(di, id); err2 != nil {
				logrus.Warnf("Failed to Deactivate %s: %s", id, err)
			}
			return nil, err
		}

		mountPath, err := GetLayerMountPath(di, id)
		if err != nil {
			if err := UnprepareLayer(di, id); err != nil {
				logrus.Warnf("Failed to Unprepare %s: %s", id, err)
			}
			if err2 := DeactivateLayer(di, id); err2 != nil {
				logrus.Warnf("Failed to Deactivate %s: %s", id, err)
			}
			return nil, err
		}
		return mountPath, nil
	}

	// V2 UVM
	logrus.Debugf("hcsshim::MountContainerLayers Is a %s V2 UVM", uvm.OperatingSystem)

	// 	Add each read-only layers. For Windows, this is a VSMB share with the ResourceUri ending in
	// a GUID based on the folder path. For Linux, this is a VPMEM device.
	//
	//  Each layer is ref-counted so that multiple containers in the same utility VM can share them.
	var vsmbAdded []string
	var vpmemAdded []string

	for _, layerPath := range layerFolders[:len(layerFolders)-1] {
		var err error
		if uvm.OperatingSystem == "windows" {
			err = uvm.AddVSMB(layerPath, hcsschemav2.VsmbFlagReadOnly|hcsschemav2.VsmbFlagPseudoOplocks|hcsschemav2.VsmbFlagTakeBackupPrivilege|hcsschemav2.VsmbFlagCacheIO|hcsschemav2.VsmbFlagShareRead)
			if err == nil {
				vsmbAdded = append(vsmbAdded, layerPath)
			}
		} else {
			_, _, err = uvm.AddVPMEM(filepath.Join(layerPath, "layer.vhd"), "", true) // ContainerPath calculated. Will be /tmp/vpmemN/
			if err == nil {
				vpmemAdded = append(vpmemAdded, layerPath)
			}
		}
		if err != nil {
			// TODO Remove VPMEM too now. And in all call sites below
			uvm.removeVSMBOnMountFailure(vsmbAdded)
			return nil, err
		}

	}

	// 	Add the sandbox at an unused SCSI location. The container path inside the utility VM will be C:\<GUID> where
	// 	GUID is based on the folder in which the sandbox is located. Therefore, it is critical that if two containers
	// 	are created in the same utility VM, they have unique sandbox directories.
	_, sandboxPath := filepath.Split(layerFolders[len(layerFolders)-1])
	containerPathGUID, err := NameToGuid(sandboxPath)
	if err != nil {
		uvm.removeVSMBOnMountFailure(vsmbAdded)
		return nil, err
	}
	hostPath := filepath.Join(layerFolders[len(layerFolders)-1], "sandbox.vhdx")

	// TODO: Different container path for linux
	containerPath := fmt.Sprintf(`C:\%s`, containerPathGUID.ToString())
	controller, lun, err := uvm.AddSCSI(hostPath, containerPath)
	if err != nil {
		uvm.removeVSMBOnMountFailure(vsmbAdded)
		return nil, err
	}

	// 	Load the filter at the C:\<GUID> location calculated above. We pass into this request each of the
	// 	read-only layer folders.
	combinedLayers := hcsschemav2.CombinedLayersV2{}
	if uvm.OperatingSystem == "windows" {
		layers := []hcsschemav2.ContainersResourcesLayerV2{}
		for _, vsmb := range vsmbAdded {
			vsmbGUID, err := uvm.GetVSMBGUID(vsmb)
			if err != nil {
				uvm.removeVSMBOnMountFailure(vsmbAdded)
				uvm.removeSCSIOnMountFailure(hostPath, controller, lun)
				return nil, err
			}
			layers = append(layers, hcsschemav2.ContainersResourcesLayerV2{
				Id:   vsmbGUID,
				Path: fmt.Sprintf(`\\?\VMSMB\VSMB-{dcc079ae-60ba-4d07-847c-3493609c0870}\%s`, vsmbGUID),
			})
		}
		combinedLayers = hcsschemav2.CombinedLayersV2{
			ContainerRootPath: fmt.Sprintf(`C:\%s`, containerPathGUID.ToString()),
			Layers:            layers,
		}
		combinedLayersModification := &hcsschemav2.ModifySettingsRequestV2{
			ResourceType:   hcsschemav2.ResourceTypeCombinedLayers,
			RequestType:    hcsschemav2.RequestTypeAdd,
			HostedSettings: combinedLayers,
		}
		if err := uvm.Modify(combinedLayersModification); err != nil {
			uvm.removeVSMBOnMountFailure(vsmbAdded)
			uvm.removeSCSIOnMountFailure(hostPath, controller, lun)
			return nil, err
		}
	}
	logrus.Debugln("hcsshim::MountContainerLayers Succeeded")
	return combinedLayers, nil
}

// UnmountOperation is used when calling Unmount() to determine what type of unmount is
// required. In V1 schema, this must be UnmountOperationAll. In V2, client can
// be more optimal and only unmount what they need which can be a minor performance
// improvement (eg if you know only one container is running in a utility VM, and
// the UVM is about to be torn down, there's no need to unmount the VSMB shares,
// just SCSI to have a consistent file system).
type UnmountOperation uint

const (
	UnmountOperationSCSI = 0x01
	UnmountOperationVSMB = 0x02
	UnmountOperationAll  = UnmountOperationSCSI | UnmountOperationVSMB
)

// UnmountContainerLayers is a helper for clients to hide all the complexity of layer unmounting
func UnmountContainerLayers(layerFolders []string, uvm *UtilityVM, op UnmountOperation) error {
	logrus.Debugln("hcsshim::UnmountContainerLayers", layerFolders)

	if uvm == nil {
		// Must be an argon - folders are mounted on the host
		if op != UnmountOperationAll {
			return fmt.Errorf("only operation supported for host-mounted folders is UnmountOperationAll")
		}
		if len(layerFolders) < 1 {
			return fmt.Errorf("need at least one layer for Unmount")
		}
		id := filepath.Base(layerFolders[len(layerFolders)-1])
		homeDir := filepath.Dir(layerFolders[len(layerFolders)-1])
		di := DriverInfo{HomeDir: homeDir}
		logrus.Debugln("hcsshim::Unmount UnprepareLayer", id)
		if err := UnprepareLayer(di, id); err != nil {
			return err
		}
		// TODO Should we try this anyway?
		logrus.Debugln("hcsshim::UnmountContainerLayers DeactivateLayer", id)
		return DeactivateLayer(di, id)
	}

	// V2 Xenon

	// Base+Sandbox as a minimum. This is different to v1 which only requires the sandbox
	if len(layerFolders) < 2 {
		return fmt.Errorf("at least two layers are required for unmount")
	}

	var retError error

	// Unload the storage filter followed by the SCSI sandbox
	if (op & UnmountOperationSCSI) == UnmountOperationSCSI {
		// TODO BUGBUG - logic error if failed to NameToGUID as containerPathGUID is used later too
		_, sandboxPath := filepath.Split(layerFolders[len(layerFolders)-1])
		containerPathGUID, err := NameToGuid(sandboxPath)
		if err != nil {
			logrus.Warnf("may leak a sandbox in %s as nametoguid failed: %s", err)
		} else {
			containerRootPath := fmt.Sprintf(`C:\%s`, containerPathGUID.ToString())
			logrus.Debugf("hcsshim::UnmountContainerLayers CombinedLayers %s", containerRootPath)
			combinedLayersModification := &hcsschemav2.ModifySettingsRequestV2{
				ResourceType:   hcsschemav2.ResourceTypeCombinedLayers,
				RequestType:    hcsschemav2.RequestTypeRemove,
				HostedSettings: hcsschemav2.CombinedLayersV2{ContainerRootPath: containerRootPath},
			}
			if err := uvm.Modify(combinedLayersModification); err != nil {
				logrus.Errorf(err.Error())
			}
		}

		// Hot remove the sandbox from the SCSI controller
		uvm.scsiLocations.Lock()
		hostSandboxFile := filepath.Join(layerFolders[len(layerFolders)-1], "sandbox.vhdx")
		controller, lun, err := uvm.findSCSIAttachment(hostSandboxFile)
		if err != nil {
			logrus.Warnf("sandbox %s is not attached to SCSI - cannot remove!", hostSandboxFile)
		} else {
			containerRootPath := fmt.Sprintf(`C:\%s`, containerPathGUID.ToString())
			logrus.Debugf("hcsshim::UnmountContainerLayers SCSI %d:%d %s %s", controller, lun, containerRootPath, hostSandboxFile)
			if err := uvm.removeSCSI(hostSandboxFile, controller, lun); err != nil {
				e := fmt.Errorf("failed to remove SCSI %s: %s", hostSandboxFile, err)
				logrus.Debugln(e)
				if retError == nil {
					retError = e
				} else {
					retError = errors.Wrapf(retError, e.Error())
				}
			}
		}
		uvm.scsiLocations.Unlock()
	}

	// Remove each of the read-only layers from VSMB. These's are ref-counted and
	// only removed once the count drops to zero. This allows multiple containers
	// to share layers.
	if len(layerFolders) > 1 && (op&UnmountOperationVSMB) == UnmountOperationVSMB {
		if uvm.vsmbShares.shares == nil {
			uvm.vsmbShares.shares = make(map[string]vsmbShare)
		}
		for _, layerPath := range layerFolders[:len(layerFolders)-1] {
			if e := uvm.RemoveVSMB(layerPath); e != nil {
				logrus.Debugln(e)
				if retError == nil {
					retError = e
				} else {
					retError = errors.Wrapf(retError, e.Error())
				}
			}
		}
	}

	// TODO (possibly) Consider deleting the container directory in the utility VM

	return retError
}

// removeVSMBOnMountFailure is a helper to roll-back any VSMB shares added to a utility VM on a failure path
// The mutex must NOT be held when calling this function.
func (uvm *UtilityVM) removeVSMBOnMountFailure(toRemove []string) {
	if len(toRemove) == 0 {
		return
	}
	uvm.vsmbShares.Lock()
	defer uvm.vsmbShares.Unlock()
	for _, vsmbShare := range toRemove {
		if err := uvm.removeVSMB(vsmbShare); err != nil {
			logrus.Warnf("Possibly leaked vsmbshare on error removal path: %s", err)
		}
	}
}

// removeSCSIOnMountFailure is a helper to roll-back a SCSI disk added to a utility VM on a failure path.
// The mutex must NOT be held when calling this function.
func (uvm *UtilityVM) removeSCSIOnMountFailure(hostPath string, controller int, lun int) {
	uvm.scsiLocations.Lock()
	defer uvm.scsiLocations.Unlock()
	if err := uvm.removeSCSI(hostPath, controller, lun); err != nil {
		logrus.Warnf("Possibly leaked SCSI disk on error removal path: %s", err)
	}
}
