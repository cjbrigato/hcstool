package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"golang.org/x/sys/windows"
)

// --- HCS v2 JSON spec structs (partially typed) ---

// ComputeSystemSpec is the top-level HCS v2 configuration. Fields we don't
// need to inspect are kept as json.RawMessage for pass-through.
type ComputeSystemSpec struct {
	Owner                              string               `json:"Owner,omitempty"`
	SchemaVersion                      *SchemaVersion       `json:"SchemaVersion,omitempty"`
	ShouldTerminateOnLastHandleClosed  bool                 `json:"ShouldTerminateOnLastHandleClosed"`
	VirtualMachine                     *VirtualMachineSpec  `json:"VirtualMachine,omitempty"`
}

type SchemaVersion struct {
	Major int `json:"Major"`
	Minor int `json:"Minor"`
}

type VirtualMachineSpec struct {
	StopOnReset bool                  `json:"StopOnReset"`
	Chipset     json.RawMessage       `json:"Chipset,omitempty"`
	ComputeTopology json.RawMessage   `json:"ComputeTopology,omitempty"`
	Devices     *DevicesSpec          `json:"Devices,omitempty"`
}

type DevicesSpec struct {
	Scsi          map[string]*ScsiController `json:"Scsi,omitempty"`
	VirtualPci    map[string]*VirtualPciDev  `json:"VirtualPci,omitempty"`
	// Pass-through fields
	EnhancedModeVideo json.RawMessage      `json:"EnhancedModeVideo,omitempty"`
	GuestInterface    json.RawMessage      `json:"GuestInterface,omitempty"`
	Keyboard          json.RawMessage      `json:"Keyboard,omitempty"`
	Mouse             json.RawMessage      `json:"Mouse,omitempty"`
	VideoMonitor      json.RawMessage      `json:"VideoMonitor,omitempty"`
}

type ScsiController struct {
	Attachments map[string]*ScsiAttachment `json:"Attachments,omitempty"`
}

type ScsiAttachment struct {
	Type   string `json:"Type"`
	Path   string `json:"Path"`
}

type VirtualPciDev struct {
	DeviceInstancePath string `json:"DeviceInstancePath,omitempty"`
	VirtualFunction    int    `json:"VirtualFunction,omitempty"`
}

// --- Enumeration result structs ---

type EnumEntry struct {
	Id           string `json:"Id"`
	SystemType   string `json:"SystemType"`
	RuntimeOsType string `json:"RuntimeOsType,omitempty"`
	State        string `json:"State"`
	Name         string `json:"Name,omitempty"`
	Owner        string `json:"Owner,omitempty"`
}

// --- VM lifecycle operations ---

// extractVHDPaths walks the spec to find all VHD(X) paths from SCSI attachments.
func extractVHDPaths(spec *ComputeSystemSpec) []string {
	var paths []string
	if spec.VirtualMachine == nil || spec.VirtualMachine.Devices == nil {
		return paths
	}
	for _, ctrl := range spec.VirtualMachine.Devices.Scsi {
		if ctrl == nil {
			continue
		}
		for _, att := range ctrl.Attachments {
			if att != nil && att.Path != "" {
				paths = append(paths, att.Path)
			}
		}
	}
	return paths
}

// makePathsAbsolute converts all VHD paths in the spec to absolute paths.
func makePathsAbsolute(spec *ComputeSystemSpec) error {
	if spec.VirtualMachine == nil || spec.VirtualMachine.Devices == nil {
		return nil
	}
	for _, ctrl := range spec.VirtualMachine.Devices.Scsi {
		if ctrl == nil {
			continue
		}
		for _, att := range ctrl.Attachments {
			if att != nil && att.Path != "" {
				abs, err := filepath.Abs(att.Path)
				if err != nil {
					return fmt.Errorf("cannot resolve path %q: %w", att.Path, err)
				}
				att.Path = abs
			}
		}
	}
	return nil
}

// injectGPU adds or replaces the VirtualPci section in the spec with GPU-PV
// devices from the provided GPU list.
func injectGPU(spec *ComputeSystemSpec, gpus []GpuDevice) {
	if spec.VirtualMachine == nil {
		spec.VirtualMachine = &VirtualMachineSpec{}
	}
	if spec.VirtualMachine.Devices == nil {
		spec.VirtualMachine.Devices = &DevicesSpec{}
	}

	pciDevs := make(map[string]*VirtualPciDev)
	for i, gpu := range gpus {
		key := fmt.Sprintf("gpu-%d", i)
		pciDevs[key] = &VirtualPciDev{
			DeviceInstancePath: gpu.InstanceID,
			VirtualFunction:    0xFFFF, // auto-assign GPU partition
		}
	}
	spec.VirtualMachine.Devices.VirtualPci = pciDevs
}

// CreateAndStartVM creates and starts a VM from a JSON spec string. It handles
// granting VM access to VHD files, and cleans up on failure.
func CreateAndStartVM(specJSON string, name string, addGPU bool) error {
	// Parse the spec
	var spec ComputeSystemSpec
	if err := json.Unmarshal([]byte(specJSON), &spec); err != nil {
		return fmt.Errorf("invalid JSON spec: %w", err)
	}

	// Set owner/name
	if spec.Owner == "" {
		spec.Owner = "hcstool"
	}

	// Resolve VHD paths to absolute
	if err := makePathsAbsolute(&spec); err != nil {
		return err
	}

	// Inject GPU if requested
	if addGPU {
		gpus, err := enumerateGPUs()
		if err != nil {
			return fmt.Errorf("GPU enumeration failed: %w", err)
		}
		if len(gpus) == 0 {
			return fmt.Errorf("no GPUs found for GPU-PV")
		}
		fmt.Fprintf(os.Stderr, "Found %d GPU(s) for GPU-PV:\n", len(gpus))
		for _, g := range gpus {
			fmt.Fprintf(os.Stderr, "  %s (%s)\n", g.Name, g.InstanceID)
		}
		injectGPU(&spec, gpus)
	}

	// Re-serialize the spec
	specBytes, err := json.Marshal(&spec)
	if err != nil {
		return fmt.Errorf("failed to serialize spec: %w", err)
	}
	finalJSON := string(specBytes)

	// Generate a GUID for this VM
	guid, err := windows.GenerateGUID()
	if err != nil {
		return fmt.Errorf("GenerateGUID failed: %w", err)
	}
	// GUID.String() returns "{...}" but HCS expects bare GUID without braces
	vmID := strings.Trim(guid.String(), "{}")

	if name != "" {
		fmt.Fprintf(os.Stderr, "Creating VM %q (ID: %s)...\n", name, vmID)
	} else {
		fmt.Fprintf(os.Stderr, "Creating VM (ID: %s)...\n", vmID)
	}

	// Grant VM access to all VHD paths
	vhdPaths := extractVHDPaths(&spec)
	var grantedPaths []string
	for _, p := range vhdPaths {
		fmt.Fprintf(os.Stderr, "  Granting VM access to %s\n", p)
		if err := grantVmAccess(vmID, p); err != nil {
			// Cleanup: revoke already-granted paths
			for _, gp := range grantedPaths {
				_ = revokeVmAccess(vmID, gp)
			}
			return fmt.Errorf("grant VM access: %w", err)
		}
		grantedPaths = append(grantedPaths, p)
	}

	// Create the compute system
	op, err := createOperation()
	if err != nil {
		revokeAll(vmID, grantedPaths)
		return err
	}

	sys, err := createComputeSystem(vmID, finalJSON, op)
	resultJSON, waitErr := waitForResult(op, infinite)
	closeOperation(op)

	if err != nil {
		revokeAll(vmID, grantedPaths)
		return err
	}
	if waitErr != nil {
		revokeAll(vmID, grantedPaths)
		if resultJSON != "" {
			fmt.Fprintf(os.Stderr, "Create result: %s\n", resultJSON)
		}
		return fmt.Errorf("create compute system: %w", waitErr)
	}

	// Start the compute system
	op2, err := createOperation()
	if err != nil {
		terminateAndClose(sys)
		revokeAll(vmID, grantedPaths)
		return err
	}

	if err := startComputeSystem(sys, op2); err != nil {
		closeOperation(op2)
		terminateAndClose(sys)
		revokeAll(vmID, grantedPaths)
		return err
	}

	_, waitErr = waitForResult(op2, infinite)
	closeOperation(op2)

	if waitErr != nil {
		terminateAndClose(sys)
		revokeAll(vmID, grantedPaths)
		return fmt.Errorf("start compute system: %w", waitErr)
	}

	// Success — close our handle (VM keeps running)
	closeComputeSystem(sys)

	// Print the VM ID to stdout for scripting
	fmt.Println(vmID)
	fmt.Fprintf(os.Stderr, "VM started successfully.\n")
	return nil
}

// terminateAndClose attempts to terminate and then close a compute system.
func terminateAndClose(sys HcsSystem) {
	op, err := createOperation()
	if err != nil {
		closeComputeSystem(sys)
		return
	}
	_ = terminateComputeSystem(sys, op)
	_, _ = waitForResult(op, 5000)
	closeOperation(op)
	closeComputeSystem(sys)
}

// revokeAll revokes VM access for all paths.
func revokeAll(vmID string, paths []string) {
	for _, p := range paths {
		_ = revokeVmAccess(vmID, p)
	}
}

// ListVMs enumerates all HCS compute systems and prints them as a table.
func ListVMs() error {
	resultJSON, err := enumerateComputeSystems()
	if err != nil {
		return err
	}

	if resultJSON == "" || resultJSON == "[]" {
		fmt.Println("No compute systems found.")
		return nil
	}

	var entries []EnumEntry
	if err := json.Unmarshal([]byte(resultJSON), &entries); err != nil {
		return fmt.Errorf("failed to parse enumeration result: %w\n  raw: %s", err, resultJSON)
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tTYPE\tSTATE\tOWNER\tNAME")
	for _, e := range entries {
		name := e.Name
		if name == "" {
			name = "-"
		}
		owner := e.Owner
		if owner == "" {
			owner = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", e.Id, e.SystemType, e.State, owner, name)
	}
	w.Flush()
	return nil
}

// InspectVM opens a compute system and prints its properties as pretty JSON.
func InspectVM(id string) error {
	sys, err := openComputeSystem(id)
	if err != nil {
		return err
	}
	defer closeComputeSystem(sys)

	propsJSON, err := getComputeSystemProperties(sys)
	if err != nil {
		return err
	}

	// Pretty-print the JSON
	var raw json.RawMessage
	if err := json.Unmarshal([]byte(propsJSON), &raw); err != nil {
		// If it's not valid JSON, just print it raw
		fmt.Println(propsJSON)
		return nil
	}
	pretty, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		fmt.Println(propsJSON)
		return nil
	}
	fmt.Println(string(pretty))
	return nil
}

// StopVM performs a graceful shutdown of a compute system.
func StopVM(id string, timeoutMs uint32) error {
	sys, err := openComputeSystem(id)
	if err != nil {
		return err
	}
	defer closeComputeSystem(sys)

	op, err := createOperation()
	if err != nil {
		return err
	}
	defer closeOperation(op)

	if err := shutdownComputeSystem(sys, op); err != nil {
		return err
	}

	_, err = waitForResult(op, timeoutMs)
	return err
}

// KillVM forcibly terminates a compute system.
func KillVM(id string) error {
	sys, err := openComputeSystem(id)
	if err != nil {
		return err
	}
	defer closeComputeSystem(sys)

	op, err := createOperation()
	if err != nil {
		return err
	}
	defer closeOperation(op)

	if err := terminateComputeSystem(sys, op); err != nil {
		return err
	}

	_, err = waitForResult(op, 10000)
	return err
}

// --- Spec builder for quick-create mode ---

func buildMinimalSpec(vhdxPath string, memoryMB, cpuCount int, gpuDevices []GpuDevice) (string, error) {
	absPath, err := filepath.Abs(vhdxPath)
	if err != nil {
		return "", fmt.Errorf("cannot resolve VHDX path: %w", err)
	}

	// Verify file exists
	if _, err := os.Stat(absPath); err != nil {
		return "", fmt.Errorf("VHDX not found: %w", err)
	}

	spec := ComputeSystemSpec{
		Owner: "hcstool",
		SchemaVersion: &SchemaVersion{Major: 2, Minor: 1},
		ShouldTerminateOnLastHandleClosed: false,
		VirtualMachine: &VirtualMachineSpec{
			StopOnReset: true,
			Chipset: json.RawMessage(`{
				"Uefi": {
					"BootThis": {
						"DevicePath": "Primary",
						"DeviceType": "ScsiDrive",
						"DiskNumber": 0
					}
				}
			}`),
			ComputeTopology: json.RawMessage(fmt.Sprintf(`{
				"Memory": {
					"SizeInMB": %d,
					"AllowOvercommit": true
				},
				"Processor": {
					"Count": %d
				}
			}`, memoryMB, cpuCount)),
			Devices: &DevicesSpec{
				Scsi: map[string]*ScsiController{
					"Primary": {
						Attachments: map[string]*ScsiAttachment{
							"0": {
								Type: "VirtualDisk",
								Path: absPath,
							},
						},
					},
				},
			},
		},
	}

	if len(gpuDevices) > 0 {
		injectGPU(&spec, gpuDevices)
	}

	data, err := json.MarshalIndent(&spec, "", "  ")
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// buildSpecFromFlags creates a JSON spec from CLI flags.
func buildSpecFromFlags(vhdxPath string, memoryMB, cpuCount int, addGPU bool) (string, error) {
	var gpuDevices []GpuDevice
	if addGPU {
		var err error
		gpuDevices, err = enumerateGPUs()
		if err != nil {
			return "", fmt.Errorf("GPU enumeration failed: %w", err)
		}
		if len(gpuDevices) == 0 {
			return "", fmt.Errorf("no GPUs found for GPU-PV")
		}
		fmt.Fprintf(os.Stderr, "Found %d GPU(s) for GPU-PV:\n", len(gpuDevices))
		for _, g := range gpuDevices {
			fmt.Fprintf(os.Stderr, "  %s (%s)\n", g.Name, g.InstanceID)
		}
	}

	return buildMinimalSpec(vhdxPath, memoryMB, cpuCount, gpuDevices)
}

// readSpecFile reads a JSON spec file and returns its contents.
func readSpecFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading spec file: %w", err)
	}

	// Validate it's valid JSON
	var raw json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return "", fmt.Errorf("spec file is not valid JSON: %w", err)
	}

	return string(data), nil
}

// printSpec prints a spec to stderr without actually creating a VM (for debugging).
func printSpec(specJSON string) {
	var raw json.RawMessage
	if err := json.Unmarshal([]byte(specJSON), &raw); err != nil {
		fmt.Fprintln(os.Stderr, specJSON)
		return
	}
	pretty, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, specJSON)
		return
	}
	fmt.Fprintln(os.Stderr, string(pretty))
}

// stringSliceContains checks if a string slice contains a value.
func stringSliceContains(slice []string, val string) bool {
	for _, s := range slice {
		if strings.EqualFold(s, val) {
			return true
		}
	}
	return false
}
