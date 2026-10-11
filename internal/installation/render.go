package installation

import (
	"errors"

	"github.com/dmedovich/spawnpod/internal/admission"
	"github.com/dmedovich/spawnpod/internal/config"
	"github.com/dmedovich/spawnpod/internal/deployment"
)

// RenderAdmission retains the legacy command using the bundle's normalization.
func RenderAdmission(source config.Config) ([]byte, error) {
	effective, err := config.InstallationConfig(source)
	if err != nil {
		return nil, err
	}
	return admission.Render(effective)
}

// RenderDeployment rejects a split runtime identity before rendering. Upgrade
// render accepts a single --image and updates only the generated configuration.
func RenderDeployment(source config.Config, controllerImage, runnerImage string) ([]byte, error) {
	if controllerImage != source.AgentImage {
		return nil, errors.New("Controller and config.agentImage must use the same runtime digest; use upgrade render --image to update them together")
	}
	effective, err := config.InstallationConfig(source)
	if err != nil {
		return nil, err
	}
	return deployment.Render(effective, controllerImage, runnerImage)
}
