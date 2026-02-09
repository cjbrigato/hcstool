package main

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// GpuDevice holds information about a GPU suitable for GPU-PV passthrough.
type GpuDevice struct {
	Name       string // Friendly device name
	InstanceID string // Device instance path (e.g., PCI\VEN_10DE&DEV_...)
}

// GUID_DEVCLASS_DISPLAY is the device setup class GUID for display adapters.
var guidDevClassDisplay = windows.GUID{
	Data1: 0x4d36e968,
	Data2: 0xe325,
	Data3: 0x11ce,
	Data4: [8]byte{0xbf, 0xc1, 0x08, 0x00, 0x2b, 0xe1, 0x03, 0x18},
}

// SetupAPI constants
const (
	digcfPresent         = 0x00000002
	digcfDeviceInterface = 0x00000010
	spdrpFriendlyName    = 0x0000000C
	spdrpDeviceDesc      = 0x00000000
)

// SP_DEVINFO_DATA for SetupAPI
type spDevinfoData struct {
	Size      uint32
	ClassGUID windows.GUID
	DevInst   uint32
	Reserved  uintptr
}

var (
	modSetupAPI = windows.NewLazySystemDLL("setupapi.dll")

	procSetupDiGetClassDevsW         = modSetupAPI.NewProc("SetupDiGetClassDevsW")
	procSetupDiEnumDeviceInfo        = modSetupAPI.NewProc("SetupDiEnumDeviceInfo")
	procSetupDiGetDeviceInstanceIdW  = modSetupAPI.NewProc("SetupDiGetDeviceInstanceIdW")
	procSetupDiGetDeviceRegistryPropertyW = modSetupAPI.NewProc("SetupDiGetDeviceRegistryPropertyW")
	procSetupDiDestroyDeviceInfoList = modSetupAPI.NewProc("SetupDiDestroyDeviceInfoList")
)

// enumerateGPUs finds all present display adapters using SetupAPI.
func enumerateGPUs() ([]GpuDevice, error) {
	// SetupDiGetClassDevs with DIGCF_PRESENT to get only present devices
	hDevInfo, _, err := procSetupDiGetClassDevsW.Call(
		uintptr(unsafe.Pointer(&guidDevClassDisplay)),
		0, // Enumerator — NULL
		0, // hwndParent — NULL
		uintptr(digcfPresent),
	)
	if hDevInfo == uintptr(windows.InvalidHandle) {
		return nil, fmt.Errorf("SetupDiGetClassDevs failed: %w", err)
	}
	defer procSetupDiDestroyDeviceInfoList.Call(hDevInfo)

	var gpus []GpuDevice

	for i := uint32(0); ; i++ {
		var devInfo spDevinfoData
		devInfo.Size = uint32(unsafe.Sizeof(devInfo))

		r1, _, _ := procSetupDiEnumDeviceInfo.Call(
			hDevInfo,
			uintptr(i),
			uintptr(unsafe.Pointer(&devInfo)),
		)
		if r1 == 0 {
			break // No more devices
		}

		// Get device instance ID
		instanceID := getDeviceInstanceID(hDevInfo, &devInfo)
		if instanceID == "" {
			continue
		}

		// Get friendly name (fall back to device description)
		name := getDeviceRegistryString(hDevInfo, &devInfo, spdrpFriendlyName)
		if name == "" {
			name = getDeviceRegistryString(hDevInfo, &devInfo, spdrpDeviceDesc)
		}
		if name == "" {
			name = "Unknown GPU"
		}

		gpus = append(gpus, GpuDevice{
			Name:       name,
			InstanceID: instanceID,
		})
	}

	return gpus, nil
}

// getDeviceInstanceID retrieves the device instance ID string.
func getDeviceInstanceID(hDevInfo uintptr, devInfo *spDevinfoData) string {
	buf := make([]uint16, 512)
	var requiredSize uint32

	r1, _, _ := procSetupDiGetDeviceInstanceIdW.Call(
		hDevInfo,
		uintptr(unsafe.Pointer(devInfo)),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)),
		uintptr(unsafe.Pointer(&requiredSize)),
	)
	if r1 == 0 {
		return ""
	}
	return windows.UTF16ToString(buf)
}

// getDeviceRegistryString retrieves a string device registry property.
func getDeviceRegistryString(hDevInfo uintptr, devInfo *spDevinfoData, property uint32) string {
	buf := make([]uint16, 256)
	var propertyRegDataType uint32
	var requiredSize uint32

	r1, _, _ := procSetupDiGetDeviceRegistryPropertyW.Call(
		hDevInfo,
		uintptr(unsafe.Pointer(devInfo)),
		uintptr(property),
		uintptr(unsafe.Pointer(&propertyRegDataType)),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)*2), // size in bytes
		uintptr(unsafe.Pointer(&requiredSize)),
	)
	if r1 == 0 {
		return ""
	}
	return windows.UTF16ToString(buf)
}
