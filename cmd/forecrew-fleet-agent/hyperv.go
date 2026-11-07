package main

// The Hyper-V provider drives PowerShell, so a build for any OS holds it; it only
// works on Windows, which `doctor` says.
import _ "github.com/forecrew/forecrew/internal/fleetagent/hyperv"
