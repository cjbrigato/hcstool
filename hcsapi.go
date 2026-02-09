package main

import (
	"fmt"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Handle types for HCS API objects.
type HcsSystem uintptr
type HcsOperation uintptr

// Well-known HRESULTs.
const (
	hcsESystemNotFound       = 0xc037010e
	hcsEHypervisorNotPresent = 0xc0351000
	eAccessDenied            = 0x80070005
)

// hresultMessages maps known HRESULT codes to human-readable messages.
var hresultMessages = map[uint32]string{
	hcsESystemNotFound:       "HCS compute system not found",
	hcsEHypervisorNotPresent: "Hypervisor is not present — enable Hyper-V",
	eAccessDenied:            "Access denied — run as Administrator",
}

// HcsError wraps an HCS API failure with the operation name, HRESULT, and
// any result document returned by the operation.
type HcsError struct {
	Op         string
	HR         uint32
	ResultJSON string
}

func (e *HcsError) Error() string {
	var sb strings.Builder
	sb.WriteString(e.Op)
	sb.WriteString(": HRESULT ")
	sb.WriteString(fmt.Sprintf("0x%08x", e.HR))
	if msg, ok := hresultMessages[e.HR]; ok {
		sb.WriteString(" (")
		sb.WriteString(msg)
		sb.WriteString(")")
	}
	if e.ResultJSON != "" {
		sb.WriteString("\n  result: ")
		sb.WriteString(e.ResultJSON)
	}
	return sb.String()
}

// INFINITE timeout value for HcsWaitForOperationResult.
const infinite = uint32(0xFFFFFFFF)

// computecore.dll proc bindings.
var (
	modComputeCore = windows.NewLazySystemDLL("computecore.dll")

	procHcsCreateOperation            = modComputeCore.NewProc("HcsCreateOperation")
	procHcsCloseOperation             = modComputeCore.NewProc("HcsCloseOperation")
	procHcsWaitForOperationResult     = modComputeCore.NewProc("HcsWaitForOperationResult")
	procHcsCreateComputeSystem        = modComputeCore.NewProc("HcsCreateComputeSystem")
	procHcsOpenComputeSystem          = modComputeCore.NewProc("HcsOpenComputeSystem")
	procHcsCloseComputeSystem         = modComputeCore.NewProc("HcsCloseComputeSystem")
	procHcsStartComputeSystem         = modComputeCore.NewProc("HcsStartComputeSystem")
	procHcsShutDownComputeSystem      = modComputeCore.NewProc("HcsShutDownComputeSystem")
	procHcsTerminateComputeSystem     = modComputeCore.NewProc("HcsTerminateComputeSystem")
	procHcsEnumerateComputeSystems    = modComputeCore.NewProc("HcsEnumerateComputeSystems")
	procHcsGetComputeSystemProperties = modComputeCore.NewProc("HcsGetComputeSystemProperties")
	procHcsGrantVmAccess              = modComputeCore.NewProc("HcsGrantVmAccess")
	procHcsRevokeVmAccess             = modComputeCore.NewProc("HcsRevokeVmAccess")
)

// hrOK checks whether an HRESULT indicates success (S_OK or S_FALSE).
func hrOK(hr uintptr) bool {
	return hr == 0 || hr == 1
}

// createOperation creates a new HCS operation handle. The caller must close it
// with closeOperation after use.
func createOperation() (HcsOperation, error) {
	// HcsCreateOperation(context, callback) -> HCS_OPERATION
	// We pass NULL for both context and callback (synchronous usage).
	r1, _, _ := procHcsCreateOperation.Call(0, 0)
	if r1 == 0 {
		return 0, fmt.Errorf("HcsCreateOperation returned NULL")
	}
	return HcsOperation(r1), nil
}

// closeOperation closes an HCS operation handle.
func closeOperation(op HcsOperation) {
	if op != 0 {
		procHcsCloseOperation.Call(uintptr(op))
	}
}

// waitForResult waits for an HCS operation to complete and returns the result
// document JSON. The operation must still be open when this is called.
func waitForResult(op HcsOperation, timeoutMs uint32) (string, error) {
	var resultPtr *uint16
	hr, _, _ := procHcsWaitForOperationResult.Call(
		uintptr(op),
		uintptr(timeoutMs),
		uintptr(unsafe.Pointer(&resultPtr)),
	)
	var resultJSON string
	if resultPtr != nil {
		resultJSON = windows.UTF16PtrToString(resultPtr)
		// The result document is owned by the operation — valid until close.
		// We copy it to a Go string above, so it's safe.
	}
	if !hrOK(hr) {
		return resultJSON, &HcsError{
			Op:         "HcsWaitForOperationResult",
			HR:         uint32(hr),
			ResultJSON: resultJSON,
		}
	}
	return resultJSON, nil
}

// createComputeSystem creates a new HCS compute system.
func createComputeSystem(id, configJSON string, op HcsOperation) (HcsSystem, error) {
	idPtr, err := windows.UTF16PtrFromString(id)
	if err != nil {
		return 0, fmt.Errorf("invalid system id: %w", err)
	}
	configPtr, err := windows.UTF16PtrFromString(configJSON)
	if err != nil {
		return 0, fmt.Errorf("invalid config JSON: %w", err)
	}

	var sys HcsSystem
	// HcsCreateComputeSystem(id, configuration, operation, securityDescriptor, computeSystem)
	hr, _, _ := procHcsCreateComputeSystem.Call(
		uintptr(unsafe.Pointer(idPtr)),
		uintptr(unsafe.Pointer(configPtr)),
		uintptr(op),
		0, // security descriptor — NULL for default
		uintptr(unsafe.Pointer(&sys)),
	)
	if !hrOK(hr) {
		return 0, &HcsError{Op: "HcsCreateComputeSystem", HR: uint32(hr)}
	}
	return sys, nil
}

// openComputeSystem opens an existing compute system by ID.
func openComputeSystem(id string) (HcsSystem, error) {
	idPtr, err := windows.UTF16PtrFromString(id)
	if err != nil {
		return 0, fmt.Errorf("invalid system id: %w", err)
	}

	var sys HcsSystem
	// HcsOpenComputeSystem(id, requestedAccess, computeSystem)
	hr, _, _ := procHcsOpenComputeSystem.Call(
		uintptr(unsafe.Pointer(idPtr)),
		uintptr(0x10000000), // GENERIC_ALL
		uintptr(unsafe.Pointer(&sys)),
	)
	if !hrOK(hr) {
		return 0, &HcsError{Op: "HcsOpenComputeSystem", HR: uint32(hr)}
	}
	return sys, nil
}

// closeComputeSystem releases the handle to a compute system. This does NOT
// stop the VM — it just releases our reference.
func closeComputeSystem(sys HcsSystem) {
	if sys != 0 {
		procHcsCloseComputeSystem.Call(uintptr(sys))
	}
}

// startComputeSystem starts a created compute system.
func startComputeSystem(sys HcsSystem, op HcsOperation) error {
	// HcsStartComputeSystem(computeSystem, operation, options)
	hr, _, _ := procHcsStartComputeSystem.Call(
		uintptr(sys),
		uintptr(op),
		0, // options — NULL
	)
	if !hrOK(hr) {
		return &HcsError{Op: "HcsStartComputeSystem", HR: uint32(hr)}
	}
	return nil
}

// shutdownComputeSystem initiates a clean shutdown of a compute system.
func shutdownComputeSystem(sys HcsSystem, op HcsOperation) error {
	// HcsShutDownComputeSystem(computeSystem, operation, options)
	hr, _, _ := procHcsShutDownComputeSystem.Call(
		uintptr(sys),
		uintptr(op),
		0,
	)
	if !hrOK(hr) {
		return &HcsError{Op: "HcsShutDownComputeSystem", HR: uint32(hr)}
	}
	return nil
}

// terminateComputeSystem forcibly stops a compute system.
func terminateComputeSystem(sys HcsSystem, op HcsOperation) error {
	// HcsTerminateComputeSystem(computeSystem, operation, options)
	hr, _, _ := procHcsTerminateComputeSystem.Call(
		uintptr(sys),
		uintptr(op),
		0,
	)
	if !hrOK(hr) {
		return &HcsError{Op: "HcsTerminateComputeSystem", HR: uint32(hr)}
	}
	return nil
}

// enumerateComputeSystems enumerates all HCS compute systems and returns
// the result JSON (an array of system descriptors).
func enumerateComputeSystems() (string, error) {
	op, err := createOperation()
	if err != nil {
		return "", err
	}
	defer closeOperation(op)

	// HcsEnumerateComputeSystems(query, operation)
	// Pass NULL query to list all.
	hr, _, _ := procHcsEnumerateComputeSystems.Call(0, uintptr(op))
	if !hrOK(hr) {
		return "", &HcsError{Op: "HcsEnumerateComputeSystems", HR: uint32(hr)}
	}

	return waitForResult(op, infinite)
}

// getComputeSystemProperties retrieves properties of a compute system.
func getComputeSystemProperties(sys HcsSystem) (string, error) {
	op, err := createOperation()
	if err != nil {
		return "", err
	}
	defer closeOperation(op)

	// HcsGetComputeSystemProperties(computeSystem, operation, propertyQuery)
	hr, _, _ := procHcsGetComputeSystemProperties.Call(
		uintptr(sys),
		uintptr(op),
		0, // NULL query = all properties
	)
	if !hrOK(hr) {
		return "", &HcsError{Op: "HcsGetComputeSystemProperties", HR: uint32(hr)}
	}

	return waitForResult(op, infinite)
}

// grantVmAccess grants a VM (by ID) access to a file on the host. The file
// path must be absolute. This is synchronous — no operation handle needed.
func grantVmAccess(vmID, filePath string) error {
	vmIDPtr, err := windows.UTF16PtrFromString(vmID)
	if err != nil {
		return fmt.Errorf("invalid VM ID: %w", err)
	}
	filePathPtr, err := windows.UTF16PtrFromString(filePath)
	if err != nil {
		return fmt.Errorf("invalid file path: %w", err)
	}

	hr, _, _ := procHcsGrantVmAccess.Call(
		uintptr(unsafe.Pointer(vmIDPtr)),
		uintptr(unsafe.Pointer(filePathPtr)),
	)
	if !hrOK(hr) {
		return &HcsError{
			Op:         fmt.Sprintf("HcsGrantVmAccess(%s)", filePath),
			HR:         uint32(hr),
		}
	}
	return nil
}

// revokeVmAccess revokes a VM's access to a file previously granted.
func revokeVmAccess(vmID, filePath string) error {
	vmIDPtr, err := windows.UTF16PtrFromString(vmID)
	if err != nil {
		return err
	}
	filePathPtr, err := windows.UTF16PtrFromString(filePath)
	if err != nil {
		return err
	}

	hr, _, _ := procHcsRevokeVmAccess.Call(
		uintptr(unsafe.Pointer(vmIDPtr)),
		uintptr(unsafe.Pointer(filePathPtr)),
	)
	if !hrOK(hr) {
		return &HcsError{Op: fmt.Sprintf("HcsRevokeVmAccess(%s)", filePath), HR: uint32(hr)}
	}
	return nil
}
