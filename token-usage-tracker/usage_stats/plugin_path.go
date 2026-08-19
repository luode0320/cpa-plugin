package usagestats

import (
	"os"
	"path/filepath"
	"strings"
)

const (
	legacyDefaultDataPath = "./data/token-usage-tracker.db"
	defaultDataFileName   = "token-usage-tracker.db"
)

func resolvedDefaultDataPath() string {
	modulePath, _ := loadedPluginPath()
	executablePath, _ := os.Executable()
	workingDir, _ := os.Getwd()
	return resolveDefaultDataPath(modulePath, executablePath, workingDir)
}

func resolveDefaultDataPath(modulePath, executablePath, workingDir string) string {
	if root, ok := cliProxyRootFromPluginPath(modulePath, workingDir); ok {
		return filepath.Join(root, "data", defaultDataFileName)
	}
	if root, ok := cliProxyRootBesideExecutable(executablePath); ok {
		return filepath.Join(root, "data", defaultDataFileName)
	}
	if root, ok := cliProxyRootAtWorkingDir(workingDir); ok {
		return filepath.Join(root, "data", defaultDataFileName)
	}
	return legacyDefaultDataPath
}

func cliProxyRootFromPluginPath(modulePath, workingDir string) (string, bool) {
	modulePath = strings.TrimSpace(modulePath)
	if modulePath == "" {
		return "", false
	}
	if !filepath.IsAbs(modulePath) {
		if strings.TrimSpace(workingDir) == "" {
			return "", false
		}
		modulePath = filepath.Join(workingDir, modulePath)
	}

	dir := filepath.Dir(filepath.Clean(modulePath))
	for {
		if strings.EqualFold(filepath.Base(dir), "plugins") {
			root := filepath.Dir(dir)
			if root != dir && filepath.Dir(root) != root {
				return root, true
			}
			return "", false
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

func cliProxyRootBesideExecutable(executablePath string) (string, bool) {
	executablePath = strings.TrimSpace(executablePath)
	if executablePath == "" {
		return "", false
	}
	return cliProxyRootWithPluginsDir(filepath.Dir(filepath.Clean(executablePath)))
}

func cliProxyRootAtWorkingDir(workingDir string) (string, bool) {
	workingDir = strings.TrimSpace(workingDir)
	if workingDir == "" {
		return "", false
	}
	absolute, err := filepath.Abs(filepath.Clean(workingDir))
	if err != nil {
		return "", false
	}
	return cliProxyRootWithPluginsDir(absolute)
}

func cliProxyRootWithPluginsDir(candidate string) (string, bool) {
	if filepath.Dir(candidate) == candidate {
		return "", false
	}
	info, err := os.Stat(filepath.Join(candidate, "plugins"))
	if err != nil || !info.IsDir() {
		return "", false
	}
	return candidate, true
}
