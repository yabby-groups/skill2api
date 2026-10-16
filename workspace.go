package main

const dockerWorkspaceDir = "/workspace"

// taskWorkspace names one task directory from both execution environments.
// The host path is the only location the worker scans or serves; Docker sees
// that same bind mount at dockerWorkspaceDir.
type taskWorkspace struct {
	hostPath string
}

func workspaceFor(outputDir string) taskWorkspace {
	return taskWorkspace{hostPath: outputDir}
}

func (w taskWorkspace) codexDir(docker bool) string {
	if docker {
		return dockerWorkspaceDir
	}
	return w.hostPath
}

func (w taskWorkspace) artifactInstruction(docker bool) string {
	if docker {
		return "Write all task materials and final deliverables under /workspace (the current working directory). Do not treat status.json as an artifact destination."
	}
	return "Write all task materials and final deliverables under the current working directory. Do not treat status.json as an artifact destination."
}
