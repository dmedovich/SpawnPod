#!/usr/bin/env python3
"""Assert the security properties of one live SpawnPod job Pod."""

import argparse
import json
import subprocess
from pathlib import Path


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--kubeconfig", type=Path, required=True)
    parser.add_argument("--id-file", type=Path, required=True)
    args = parser.parse_args()
    environment_id = args.id_file.read_text().strip()
    if len(environment_id) != 32 or any(c not in "0123456789abcdef" for c in environment_id):
        parser.error("id-file must contain a SpawnPod environment ID")

    result = subprocess.run(
        ["kubectl", "--kubeconfig", str(args.kubeconfig), "get", "pod",
         "sp-" + environment_id, "-n", "ci", "-o", "json"],
        capture_output=True, text=True, check=True,
    )
    pod = json.loads(result.stdout)
    spec = pod["spec"]
    assert pod["status"]["phase"] == "Running"
    assert pod["metadata"]["labels"]["spawnpod.dev/environment"] == environment_id
    assert spec["serviceAccountName"] == "spawnpod-job"
    assert spec["automountServiceAccountToken"] is False
    assert spec["nodeSelector"]["kubernetes.io/arch"] == "amd64"
    assert spec["securityContext"]["runAsNonRoot"] is True
    assert spec["securityContext"]["runAsUser"] == 10001
    assert spec["securityContext"]["seccompProfile"]["type"] == "RuntimeDefault"
    assert spec.get("imagePullSecrets") is None
    assert len(spec["volumes"]) == 3
    assert all("emptyDir" in volume for volume in spec["volumes"])
    workspace = next(volume for volume in spec["volumes"] if volume["name"] == "workspace")
    assert workspace["emptyDir"]["sizeLimit"] == "2Gi"
    for container in spec["initContainers"] + spec["containers"]:
        security = container["securityContext"]
        assert security["runAsNonRoot"] is True
        assert security["runAsUser"] == 10001
        assert security["allowPrivilegeEscalation"] is False
        assert security["readOnlyRootFilesystem"] is True
        assert security["capabilities"]["drop"] == ["ALL"]
        assert security["seccompProfile"]["type"] == "RuntimeDefault"
        assert not container.get("env") and not container.get("envFrom")
    print(f"PASS live Pod {pod['metadata']['name']}: bounded emptyDir, non-root, tokenless, no Secret mounts")


if __name__ == "__main__":
    main()
