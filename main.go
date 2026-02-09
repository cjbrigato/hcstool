package main

import (
	"flag"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func usage() {
	fmt.Fprintf(os.Stderr, `hcstool — HCS VM Lifecycle Tool

Usage:
  hcstool create --spec file.json [--gpu] [--name myvm]
  hcstool create --vhdx boot.vhdx [--memory 2048] [--cpus 2] [--gpu] [--name myvm]
  hcstool list
  hcstool inspect <vm-id>
  hcstool dump <vm-id>
  hcstool stop <vm-id> [--timeout 30]
  hcstool kill <vm-id>

Commands:
  create    Create and start a VM from a JSON spec or VHDX file
  list      List all HCS compute systems
  inspect   Show basic properties of a compute system
  dump      Dump all available properties (memory, devices, stats, etc.)
  stop      Gracefully shut down a compute system
  kill      Forcibly terminate a compute system
`)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	// Admin elevation check
	token := windows.GetCurrentProcessToken()
	elevated := token.IsElevated()
	if !elevated {
		fmt.Fprintln(os.Stderr, "Warning: not running as Administrator. HCS operations require elevation.")
	}

	cmd := os.Args[1]
	switch cmd {
	case "create":
		cmdCreate(os.Args[2:])
	case "list":
		cmdList()
	case "inspect":
		cmdInspect(os.Args[2:])
	case "dump":
		cmdDump(os.Args[2:])
	case "stop":
		cmdStop(os.Args[2:])
	case "kill":
		cmdKill(os.Args[2:])
	case "help", "--help", "-h":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n\n", cmd)
		usage()
		os.Exit(1)
	}
}

func cmdCreate(args []string) {
	fs := flag.NewFlagSet("create", flag.ExitOnError)
	specFile := fs.String("spec", "", "Path to HCS v2 JSON spec file")
	vhdxPath := fs.String("vhdx", "", "Path to bootable VHDX file (quick-create mode)")
	memoryMB := fs.Int("memory", 2048, "Memory in MB (quick-create mode)")
	cpuCount := fs.Int("cpus", 2, "Number of virtual CPUs (quick-create mode)")
	gpu := fs.Bool("gpu", false, "Enable GPU-PV passthrough")
	name := fs.String("name", "", "Friendly name for the VM")
	dryRun := fs.Bool("dry-run", false, "Print the generated spec without creating the VM")
	fs.Parse(args)

	if *specFile == "" && *vhdxPath == "" {
		fmt.Fprintln(os.Stderr, "Error: specify either --spec or --vhdx")
		fs.Usage()
		os.Exit(1)
	}

	if *specFile != "" && *vhdxPath != "" {
		fmt.Fprintln(os.Stderr, "Error: --spec and --vhdx are mutually exclusive")
		os.Exit(1)
	}

	var specJSON string
	var err error

	if *specFile != "" {
		specJSON, err = readSpecFile(*specFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	} else {
		specJSON, err = buildSpecFromFlags(*vhdxPath, *memoryMB, *cpuCount, *gpu)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		// GPU already injected by buildSpecFromFlags, don't inject again
		*gpu = false
	}

	if *dryRun {
		printSpec(specJSON)
		return
	}

	if err := CreateAndStartVM(specJSON, *name, *gpu); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func cmdList() {
	if err := ListVMs(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func cmdInspect(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "Usage: hcstool inspect <vm-id>")
		os.Exit(1)
	}
	if err := InspectVM(args[0]); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func cmdDump(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "Usage: hcstool dump <vm-id>")
		os.Exit(1)
	}
	if err := DumpVM(args[0]); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func cmdStop(args []string) {
	fs := flag.NewFlagSet("stop", flag.ExitOnError)
	timeout := fs.Int("timeout", 30, "Shutdown timeout in seconds")
	fs.Parse(args)

	remaining := fs.Args()
	if len(remaining) < 1 {
		fmt.Fprintln(os.Stderr, "Usage: hcstool stop <vm-id> [--timeout 30]")
		os.Exit(1)
	}

	timeoutMs := uint32(*timeout * 1000)
	if err := StopVM(remaining[0], timeoutMs); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "Compute system shut down successfully.")
}

func cmdKill(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "Usage: hcstool kill <vm-id>")
		os.Exit(1)
	}
	if err := KillVM(args[0]); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "Compute system terminated.")
}
