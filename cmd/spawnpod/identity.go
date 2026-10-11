package main

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"runtime"

	"github.com/dmedovich/spawnpod/internal/config"
)

type startupIdentity struct{ Version, BinarySHA256, EffectiveConfigSHA256, Architecture string }

// Stream the executable hash so comparing host/container identities does not
// allocate another full binary under the Controller's memory limit.
func identity(cfg config.Config) startupIdentity {
	result := startupIdentity{Version: version, BinarySHA256: "unavailable", EffectiveConfigSHA256: "unavailable", Architecture: runtime.GOARCH}
	if data, err := config.InstallationBytes(cfg); err == nil {
		result.EffectiveConfigSHA256 = fmt.Sprintf("%x", sha256.Sum256(data))
	}
	if file, err := os.Open("/proc/self/exe"); err == nil {
		defer func() { _ = file.Close() }()
		hash := sha256.New()
		if _, err := io.Copy(hash, file); err == nil {
			result.BinarySHA256 = fmt.Sprintf("%x", hash.Sum(nil))
		}
	}
	return result
}
