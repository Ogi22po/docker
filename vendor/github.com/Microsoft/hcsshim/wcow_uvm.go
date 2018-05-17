// +build windows

package hcsshim

// Containers functions relating to a WCOW utility VM (implying v2)

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Microsoft/hcsshim/schema/v2"
	"github.com/Microsoft/hcsshim/schemaversion"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/sirupsen/logrus"
)

// CreateWCOWUVMSandbox is a helper to create a sandbox for a Windows utility VM
// with permissions to the specified VM ID in a specified directory
func CreateWCOWUVMSandbox(imagePath, destDirectory, vmID string) error {
	sourceSandbox := filepath.Join(imagePath, `UtilityVM\SystemTemplate.vhdx`)
	targetSandbox := filepath.Join(destDirectory, "sandbox.vhdx")
	if err := CopyFile(sourceSandbox, targetSandbox, true); err != nil {
		return err
	}
	if err := GrantVmAccess(vmID, targetSandbox); err != nil {
		// TODO: Delete the file?
		return err
	}
	return nil
}

// UVMResourcesFromContainerSpec takes a container spec and generates a
// resources structure suitable for creating a utility VM to host the container.
// This is really only relevant for a client that is running a single container
// in a utility VM using the v2 schema. It implements logic which for the v1 schema
// was implemented internally in HCS.
func UVMResourcesFromContainerSpec(spec *specs.Spec) (*specs.WindowsResources, error) {
	// TODO: Processors. File bug. V2 schema for VM doesn't allow weight/limit, just on compute system.

	if spec == nil && spec.Linux != nil { // TODO
		return nil, fmt.Errorf("UVMResourcesFromContainerSpec not supported for LCOW yet")
	}

	if spec == nil || spec.Windows == nil {
		return nil, fmt.Errorf("invalid spec")
	}
	var uvmCPUCount uint64 = 2
	var uvmMemoryMB uint64 = 512
	uvmResources := &specs.WindowsResources{
		Memory: &specs.WindowsMemoryResources{},
		CPU:    &specs.WindowsCPUResources{Count: &uvmCPUCount},
	}
	if numCPU() == 1 {
		uvmCPUCount = 1
	}
	if spec.Windows.Resources != nil {
		if spec.Windows.Resources.CPU != nil && spec.Windows.Resources.CPU.Count != nil {
			uvmCPUCount = *spec.Windows.Resources.CPU.Count
		}
		if spec.Windows.Resources.Memory.Limit != nil {
			uvmMemoryMB = (*spec.Windows.Resources.Memory.Limit) / 1024 / 1024
		}
	}

	// Add 256MB and round up to nearest 512MB
	uvmMemoryMB += 256
	if uvmMemoryMB%512 > 0 {
		uvmMemoryMB += (512 - (uvmMemoryMB % 512))
	}
	uvmMemoryBytes := uvmMemoryMB * 1024 * 1024
	uvmResources.Memory.Limit = &uvmMemoryBytes

	logrus.Debugf("hcsshim: uvmResources: Memory %d MB CPUs %d", uvmMemoryMB, *uvmResources.CPU.Count)

	return uvmResources, nil
}

// LocateWCOWUVMFolderFromLayerFolders searches a set of layer folders to determine the "uppermost"
// layer which has a utility VM image. The order of the layers is (for historical) reasons
// Read-only-layers followed by an optional read-write layer. The RO layers are in reverse
// order so that the upper-most RO layer is at the start, and the base OS layer is the
// end.
func LocateWCOWUVMFolderFromLayerFolders(layerFolders []string) (string, error) {
	var uvmFolder string
	index := 0
	for _, layerFolder := range layerFolders {
		_, err := os.Stat(filepath.Join(layerFolder, `UtilityVM`))
		if err == nil {
			uvmFolder = layerFolder
			break
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		index++
	}
	if uvmFolder == "" {
		return "", fmt.Errorf("utility VM folder could not be found in layers")
	}
	logrus.Debugf("hcsshim::LocateWCOWUVMFolderFromLayerFolders Index %d of %d possibles (%s)", index, len(layerFolders), uvmFolder)
	return uvmFolder, nil
}

func createWCOWv2UVM(coi *createOptionsExInternal) (Container, error) {
	logrus.Debugf("hcsshim::createWCOWv2UVM Creating utility VM id=%s", coi.actualId)

	iocis := "invalid OCI spec:"
	if len(coi.Spec.Windows.LayerFolders) < 2 {
		return nil, fmt.Errorf("%s Windows.LayerFolders must have length of at least 2 for a hosting system", iocis)
	}
	if len(coi.Spec.Hostname) > 0 {
		return nil, fmt.Errorf("%s Hostname cannot be set for a hosting system", iocis)
	}
	if coi.Spec.Windows.Resources != nil && coi.Spec.Windows.Resources.CPU != nil && coi.Spec.Windows.Resources.CPU.Shares != nil {
		return nil, fmt.Errorf("%s Windows.Resources.CPU.Shares must not be set for a hosting system", iocis)
	}
	if coi.Spec.Windows.Resources != nil && coi.Spec.Windows.Resources.CPU != nil && coi.Spec.Windows.Resources.CPU.Maximum != nil {
		return nil, fmt.Errorf("%s Windows.Resources.CPU.Maximum must not be set for a hosting system", iocis)
	}
	if coi.Spec.Root != nil {
		return nil, fmt.Errorf("%s Root must not be set for a hosting system", iocis)
	}
	if coi.Spec.Windows.Resources != nil && coi.Spec.Windows.Resources.Storage != nil {
		return nil, fmt.Errorf("%s Windows.Resources.Storage must not be set for a hosting system", iocis)
	}
	if coi.Spec.Windows.CredentialSpec != nil {
		return nil, fmt.Errorf("%s Windows.CredentialSpec must not be set for a hosting system", iocis)
	}
	if coi.Spec.Windows.Network != nil {
		return nil, fmt.Errorf("%s Windows.Network must not be set for a hosting system", iocis) // Need to revisit, but blocking everything currently not hooked up
	}
	if 0 != len(coi.Spec.Mounts) {
		return nil, fmt.Errorf("%s Mounts must not be set for a hosting system", iocis)
	}

	uvmFolder, err := LocateWCOWUVMFolderFromLayerFolders(coi.Spec.Windows.LayerFolders)
	if err != nil {
		return nil, fmt.Errorf("failed to locate utility VM folder from layer folders: %s", err)
	}

	// Create the sandbox in the top-most layer folder, creating the folder if it doesn't already exist.
	sandboxFolder := coi.Spec.Windows.LayerFolders[len(coi.Spec.Windows.LayerFolders)-1]
	logrus.Debugf("hcsshim::createWCOWv2UVM Sandbox folder: %s", sandboxFolder)

	// Create the directory if it doesn't exist
	if _, err := os.Stat(sandboxFolder); os.IsNotExist(err) {
		logrus.Debugf("hcsshim::createWCOWv2UVM Creating folder: %s ", sandboxFolder)
		if err := os.MkdirAll(sandboxFolder, 0777); err != nil {
			return nil, fmt.Errorf("failed to create utility VM sandbox folder: %s", err)
		}
	}

	// Create sandbox.vhdx in the sandbox folder based on the template, granting the correct permissions to it
	if err := CreateWCOWUVMSandbox(uvmFolder, sandboxFolder, coi.actualId); err != nil {
		return nil, fmt.Errorf("failed to create UVM sandbox: %s", err)
	}

	attachments := make(map[string]hcsschemav2.VirtualMachinesResourcesStorageAttachmentV2)
	attachments["0"] = hcsschemav2.VirtualMachinesResourcesStorageAttachmentV2{
		Path: filepath.Join(sandboxFolder, "sandbox.vhdx"),
		Type: "VirtualDisk",
	}
	scsi := make(map[string]hcsschemav2.VirtualMachinesResourcesStorageScsiV2)
	scsi["0"] = hcsschemav2.VirtualMachinesResourcesStorageScsiV2{Attachments: attachments}
	memory := int32(1024)
	processors := int32(2)
	if numCPU() == 1 {
		processors = 1
	}
	if coi.Spec.Windows.Resources != nil {
		if coi.Spec.Windows.Resources.Memory != nil && coi.Spec.Windows.Resources.Memory.Limit != nil {
			memory = int32(*coi.Spec.Windows.Resources.Memory.Limit / 1024 / 1024) // OCI spec is in bytes. HCS takes MB
		}
		if coi.Spec.Windows.Resources.CPU != nil && coi.Spec.Windows.Resources.CPU.Count != nil {
			processors = int32(*coi.Spec.Windows.Resources.CPU.Count)
		}
	}
	uvm := &hcsschemav2.ComputeSystemV2{
		Owner:         coi.actualOwner,
		SchemaVersion: coi.actualSchemaVersion,
		VirtualMachine: &hcsschemav2.VirtualMachineV2{
			Chipset: &hcsschemav2.VirtualMachinesResourcesChipsetV2{
				UEFI: &hcsschemav2.VirtualMachinesResourcesUefiV2{
					BootThis: &hcsschemav2.VirtualMachinesResourcesUefiBootEntryV2{
						DevicePath: `\EFI\Microsoft\Boot\bootmgfw.efi`,
						DiskNumber: 0,
						UefiDevice: "VMBFS",
					},
				},
			},
			ComputeTopology: &hcsschemav2.VirtualMachinesResourcesComputeTopologyV2{
				Memory: &hcsschemav2.VirtualMachinesResourcesComputeMemoryV2{
					Backing:             "Virtual",
					Startup:             memory,
					DirectFileMappingMB: 1024, // Sensible default, but could be a tuning parameter somewhere
				},
				Processor: &hcsschemav2.VirtualMachinesResourcesComputeProcessorV2{
					Count: processors,
				},
			},

			Devices: &hcsschemav2.VirtualMachinesDevicesV2{
				// Add networking here.... TODO
				SCSI: scsi,
				VirtualSMBShares: []hcsschemav2.VirtualMachinesResourcesStorageVSmbShareV2{hcsschemav2.VirtualMachinesResourcesStorageVSmbShareV2{
					Flags: hcsschemav2.VsmbFlagReadOnly | hcsschemav2.VsmbFlagPseudoOplocks | hcsschemav2.VsmbFlagTakeBackupPrivilege | hcsschemav2.VsmbFlagCacheIO | hcsschemav2.VsmbFlagShareRead,
					Name:  "os",
					Path:  filepath.Join(uvmFolder, `UtilityVM\Files`),
				}},
				GuestInterface: &hcsschemav2.VirtualMachinesResourcesGuestInterfaceV2{ConnectToBridge: true},
			},
		},
	}

	uvmb, err := json.Marshal(uvm)
	if err != nil {
		return nil, err
	}
	uvmContainer, err := createContainer(coi.actualId, string(uvmb), schemaversion.SchemaV20())
	if err != nil {
		logrus.Debugln("failed to create UVM: ", err)
		return nil, err
	}
	uvmContainer.(*container).scsiLocations.hostPath[0][0] = attachments["0"].Path
	uvmContainer.(*container).operatingSystem = "windows"
	return uvmContainer, nil
}
