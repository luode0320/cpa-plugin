//go:build !cgo || (!darwin && !linux)

package usagestats

func loadedPluginPath() (string, bool) {
	return "", false
}
