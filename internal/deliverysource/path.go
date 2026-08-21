package deliverysource

import "path/filepath"

const DirectoryName = ".delivery-sources"

func SharedRoot(workspaceRoot string) string {
	return filepath.Join(filepath.Dir(filepath.Clean(workspaceRoot)), DirectoryName)
}

func RevisionPathForWorkspace(workspacePath, sessionID, revisionRoundID string) string {
	return filepath.Join(SharedRoot(filepath.Dir(filepath.Clean(workspacePath))), sessionID, revisionRoundID+".git")
}
