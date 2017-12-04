// Copyright 2016-2017 VMware, Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// This is the filesystem interface for mounting volumes on a linux guest.

package fs

import (
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	// FstypeDefault contains the default FS to be used when not specified by the user.
	FstypeDefault = "ext4"

	sleepBeforeMount = 1 * time.Second          // time to sleep in case of watch failure
	diskPathByDevID  = "/dev/disk/by-id/wwn-0x" // Path for devices named by ID
	scsiHostPath     = "/sys/class/scsi_host/"  // Path for scsi hosts
	bdevPath         = "/sys/block/"
	deleteFile       = "/device/delete"
	watchPath        = "/dev/disk/by-id"
	linuxMountsFile  = "/proc/mounts" // Path of file containing linux mounts information
)

// BinSearchPath contains search paths for host binaries
var BinSearchPath = []string{"/bin", "/sbin", "/usr/bin", "/usr/sbin"}

// DevAttachWaitFallback performs basic fallback in case of watch failure.
func DevAttachWaitFallback() {
	time.Sleep(sleepBeforeMount)
}

// Mkfs creates a filesystem at the specified volDev.
func Mkfs(fstype string, label string, volDev *VolumeDevSpec) error {
	device, err := GetDevicePath(volDev)
	if err != nil {
		log.WithFields(log.Fields{"volDev": *volDev, "err": err}).Error("Failed to get device path ")
		return err
	}
	return MkfsByDevicePath(fstype, label, device)
}

// MkfsByDevicePath creates a filesystem at the specified device.
func MkfsByDevicePath(fstype string, label string, device string) error {
	var err error
	var out []byte

	// Identify mkfscmd for fstype
	mkfscmd := mkfsLookup()[fstype]

	// Workaround older versions of e2fsprogs, issue 629.
	// If mkfscmd is of an ext* filesystem use -F flag
	// to avoid having mkfs command to expect user confirmation.
	if strings.Split(mkfscmd, ".")[1][0:3] == "ext" {
		out, err = exec.Command(mkfscmd, "-F", "-L", label, device).CombinedOutput()
	} else {
		out, err = exec.Command(mkfscmd, "-L", label, device).CombinedOutput()
	}
	if err != nil {
		return fmt.Errorf("Failed to create filesystem on %s: %s. Output = %s",
			device, err, out)
	}
	return nil
}

// VerifyFSSupport checks whether the fstype filesystem is supported.
func VerifyFSSupport(fstype string) error {
	supportedFs := mkfsLookup()
	_, result := supportedFs[fstype]
	if result == false {
		msg := "Not found mkfs for " + fstype + "\nSupported filesystems found: "
		validfs := ""
		for fs := range supportedFs {
			if validfs != "" {
				validfs += ", " + fs
			} else {
				validfs += fs
			}
		}
		msg += validfs
		return errors.New(msg)
	}
	return nil
}

// mkfsLookup finds existent filesystem tools
func mkfsLookup() map[string]string {
	supportedFs := make(map[string]string)
	for _, sp := range BinSearchPath {
		mkftools, _ := filepath.Glob(sp + "/mkfs.*")
		for _, mkfs := range mkftools {
			supportedFs[strings.Split(mkfs, ".")[1]] = mkfs
		}
	}
	return supportedFs
}

// Mount the filesystem (`fs`) on the volDev at the given mountpoint.
func Mount(mountpoint string, fstype string, volDev *VolumeDevSpec, isReadOnly bool) error {
	device, err := GetDevicePath(volDev)
	if err != nil {
		log.WithFields(log.Fields{"volDev": *volDev, "err": err}).Error("Failed to get device path ")
		return err
	}
	return MountByDevicePath(mountpoint, fstype, device, isReadOnly)
}

// MountByDevicePath mounts the filesystem (`fs`) on the device at the given mount point.
func MountByDevicePath(mountpoint string, fstype string, device string, isReadOnly bool) error {
	log.WithFields(log.Fields{
		"device":     device,
		"fstype":     fstype,
		"mountpoint": mountpoint,
	}).Debug("Calling syscall.Mount() ")

	flags := 0
	if isReadOnly {
		flags = syscall.MS_RDONLY
	}
	err := syscall.Mount(device, mountpoint, fstype, uintptr(flags), "")
	if err != nil {
		return fmt.Errorf("Failed to mount device %s at %s: %s", device, mountpoint, err)
	}
	return nil
}

// MountWithID - mount device with ID
func MountWithID(mountpoint string, fstype string, id string, isReadOnly bool) error {
	log.WithFields(log.Fields{
		"device ID":  id,
		"fstype":     fstype,
		"mountpoint": mountpoint,
	}).Debug("Calling syscall.Mount() ")

	// Scan so we may have the device before attempting a mount
	// Loop over all hosts and scan each one
	device, err := GetDevicePathByID(id)
	if err != nil {
		return fmt.Errorf("Invalid device path %s for %s: %s",
			device, mountpoint, err)
	}

	flags := 0
	if isReadOnly {
		flags = syscall.MS_RDONLY
	}
	err = syscall.Mount(device, mountpoint, fstype, uintptr(flags), "")
	if err != nil {
		return fmt.Errorf("Failed to mount device %s at %s fstype %s: %s",
			device, mountpoint, fstype, err)
	}
	return nil
}

// Unmount a device from the given mount point.
func Unmount(mountPoint string) error {
	err := syscall.Unmount(mountPoint, 0)
	if err != nil {
		return fmt.Errorf("Unmount device at %s failed: %s",
			mountPoint, err)
	}
	return nil
}

func makeDevicePathWithID(id string) string {
	return diskPathByDevID + strings.Join(strings.Split(id, "-"), "")
}

// GetDevicePathByID - return full path for device with given ID
func GetDevicePathByID(id string) (string, error) {
	hosts, err := ioutil.ReadDir(scsiHostPath)
	if err != nil {
		return "", err
	}
	time.Sleep(10)
	for _, host := range hosts {
		//Scan so we may have the device before attempting a mount
		scanHost := scsiHostPath + host.Name() + "/scan"
		bytes := []byte("- - -")
		log.WithFields(log.Fields{"disk id": id, "scan cmd": scanHost}).Info("Rescanning ... ")
		err = ioutil.WriteFile(scanHost, bytes, 0644)
		if err != nil {
			return "", err
		}
	}

	watcher, errWatch := devAttachWaitPrep(watchPath)

	device := makeDevicePathWithID(id)

	if errWatch != nil {
		time.Sleep(10)
	} else {
		// Wait for the attach to complete, may timeout
		// in which case we continue creating the file system.
		devAttachWait(watcher, device)
	}
	_, err = os.Stat(device)
	if err != nil {
		return "", err
	}
	return device, nil
}

// DeleteDevicePathWithID - delete device with given ID
func DeleteDevicePathWithID(id string) error {
	// Delete the device node
	device := makeDevicePathWithID(id)
	dev, err := os.Readlink(device)
	if err != nil {
		return fmt.Errorf("Failed to read sym link for %s: %s",
			device, err)
	}
	links := strings.Split(dev, "/")
	node := bdevPath + links[len(links)-1] + deleteFile
	bytes := []byte("1")

	log.Debugf("Deleteing device node - id: %s, node: %s", id, node)
	err = ioutil.WriteFile(node, bytes, 0644)
	if err != nil {
		return err
	}
	return nil
}

// GetMountInfo returns a map of mounted volumes and devices if available. It creates a map
// of all volumes that are in use or may have been in use earlier and creates the map of
// volume to device.
func GetMountInfo(mountRoot string) (map[string]string, error) {
	volumeMountMap := make(map[string]string) // map [volume mount path] -> device

	// Read current mounted filesystems
	data, err := ioutil.ReadFile(linuxMountsFile)
	if err != nil {
		log.Errorf("Can't get info from %s (%v)", linuxMountsFile, err)
		return volumeMountMap, err
	}
	log.WithFields(log.Fields{"data": string(data)}).Debug("Mounts read successfully: ")

	for _, line := range strings.Split(string(data), lf) {
		field := strings.Fields(line)
		if len(field) < 2 {
			continue // skip empty line and lines too short to have our mount
		}
		// fields format: [/dev/sdb /mnt/vmdk/vol1 ext2 rw,relatime 0 0]
		if filepath.Dir(field[1]) != mountRoot {
			continue
		}
		vname := strings.Replace(filepath.Base(field[1]), "\\040", " ", -1)
		volumeMountMap[vname] = field[0]
	}

	log.WithFields(log.Fields{"map": volumeMountMap}).Debug("Successfully retrieved mounts: ")
	return volumeMountMap, nil
}
