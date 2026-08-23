package hostsetup

import "strings"

const dockerDesktopInstallerEnvironment = "WORKFLOW_DOCKER_INSTALLER"
const dockerDesktopExecutableEnvironment = "WORKFLOW_DOCKER_DESKTOP_EXE"

func dockerDesktopInstallerCommand() string {
	return `$p=Start-Process -FilePath $env:WORKFLOW_DOCKER_INSTALLER -ArgumentList 'install','--quiet','--accept-license' -Verb RunAs -Wait -PassThru -WindowStyle Hidden; exit $p.ExitCode`
}

func dockerDesktopStartCommand() string {
	return `Start-Process -FilePath $env:WORKFLOW_DOCKER_DESKTOP_EXE -WindowStyle Hidden`
}

func dockerDesktopEnvironment(environment []string, name, path string) []string {
	prefix := name + "="
	result := make([]string, 0, len(environment)+1)
	for _, value := range environment {
		key, _, _ := strings.Cut(value, "=")
		if !strings.EqualFold(key, name) {
			result = append(result, value)
		}
	}
	return append(result, prefix+path)
}
