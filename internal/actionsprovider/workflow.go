// Package actionsprovider serves the command jobs of pool actions on
// GitHub-hosted runners: it leases them like a worker would, dispatches the
// forecrew-matrix workflow once per job, follows the run, and turns its
// artifact into the test runs a worker would have reported (docs/actions.md).
package actionsprovider

import (
	"fmt"
	"io/fs"
)

// The workflow every project that uses pool actions has installed.
const (
	// WorkflowFile is the file name the provider dispatches.
	WorkflowFile = "forecrew-matrix.yml"
	// WorkflowPath is where the workflow lives in the project's repo.
	WorkflowPath = ".github/workflows/" + WorkflowFile
	// TemplatePath is the template in the embedded packs.
	TemplatePath = "packs/workflows/" + WorkflowFile
	// ArtifactPrefix starts the name of the artifact a run uploads; the job
	// id follows.
	ArtifactPrefix = "forecrew-"
	// ResultFile is the file in the run's artifact that says how the
	// command went.
	ResultFile = "forecrew-out/result.json"
	// RootMarker is an empty file the workflow uploads next to the results.
	// It sits at the workspace root, which pins the root of the artifact
	// there (upload-artifact roots a multi-path upload at the least common
	// ancestor of its paths), so every name in it is relative to the
	// workspace whatever the reports and artifact globs are.
	RootMarker = "forecrew-root.txt"
)

// Template returns the workflow to install, from the packs in files.
func Template(files fs.FS) ([]byte, error) {
	data, err := fs.ReadFile(files, TemplatePath)
	if err != nil {
		return nil, fmt.Errorf("read workflow template: %w", err)
	}
	return data, nil
}
