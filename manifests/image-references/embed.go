package imagereferences

import (
	_ "embed"
)

// JobsYAML contains the embedded image configuration
//
//go:embed jobs.yaml
var JobsYAML string
