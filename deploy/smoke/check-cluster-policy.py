#!/usr/bin/env python3
"""Exercise live K3s admission and ResourceQuota without leaving workloads."""

import argparse
import json
import re
import subprocess
import time
import uuid
from pathlib import Path


def kubectl(kubeconfig, *args, obj=None):
    command = ["kubectl", "--kubeconfig", str(kubeconfig), *args]
    result = subprocess.run(
        command,
        input=None if obj is None else json.dumps(obj),
        text=True,
        capture_output=True,
        check=False,
    )
    return result


def require_success(result, description):
    if result.returncode:
        raise RuntimeError(f"{description}: {result.stderr.strip()}")
    return result


def config_image(config, pattern):
    match = re.search(pattern, config, re.MULTILINE)
    if not match:
        raise RuntimeError(f"image field missing: {pattern}")
    return match.group(1)


def pod(name, agent_image, job_image):
    resources = {
        "requests": {"cpu": "250m", "memory": "256Mi", "ephemeral-storage": "1Gi"},
        "limits": {"cpu": "1", "memory": "1Gi", "ephemeral-storage": "4Gi"},
    }
    security = {
        "runAsNonRoot": True,
        "runAsUser": 10001,
        "runAsGroup": 10001,
        "allowPrivilegeEscalation": False,
        "readOnlyRootFilesystem": True,
        "capabilities": {"drop": ["ALL"]},
        "seccompProfile": {"type": "RuntimeDefault"},
    }
    return {
        "apiVersion": "v1",
        "kind": "Pod",
        "metadata": {
            "name": name,
            "namespace": "ci",
            "labels": {
                "spawnpod.dev/managed": "true",
                "spawnpod.dev/network": "isolated",
                "spawnpod.dev/environment": uuid.uuid4().hex,
            },
        },
        "spec": {
            "serviceAccountName": "spawnpod-job",
            "automountServiceAccountToken": False,
            "restartPolicy": "Never",
            "securityContext": {"runAsNonRoot": True, "seccompProfile": {"type": "RuntimeDefault"}},
            "volumes": [{"name": "workspace", "emptyDir": {}}],
            "initContainers": [{
                "name": "install-agent",
                "image": agent_image,
                "command": ["/spawnpod-agent", "install", "/tools/agent"],
                "securityContext": dict(security),
                "resources": resources,
            }],
            "containers": [{
                "name": "job",
                "image": job_image,
                "command": ["/opt/spawnpod/agent", "serve"],
                "securityContext": dict(security),
                "resources": resources,
            }],
        },
    }


def expect_denied(kubeconfig, obj, marker, description):
    result = kubectl(kubeconfig, "create", "--dry-run=server", "-f", "-", obj=obj)
    if result.returncode == 0 or marker.lower() not in result.stderr.lower():
        raise RuntimeError(
            f"{description}: expected denial containing {marker!r}; "
            f"exit={result.returncode}; stderr={result.stderr.strip()}"
        )
    print(f"PASS {description}: {result.stderr.strip().splitlines()[0]}")


def configmap(name):
    return {
        "apiVersion": "v1",
        "kind": "ConfigMap",
        "metadata": {"name": name, "namespace": "ci", "labels": {"spawnpod.dev/smoke": "quota"}},
        "data": {"purpose": "temporary quota check"},
    }


def check_rbac(kubeconfig):
    checks = [
        ("system:serviceaccount:infra:spawnpod-controller", "create", "pods", "ci", True),
        ("system:serviceaccount:infra:spawnpod-controller", "get", "secrets", "ci", False),
        ("system:serviceaccount:infra:spawnpod-controller", "create", "pods", "default", False),
        ("system:serviceaccount:ci:spawnpod-job", "get", "secrets", "ci", False),
        ("system:serviceaccount:ci:spawnpod-job", "list", "pods", "ci", False),
    ]
    for identity, verb, resource, namespace, allowed in checks:
        result = kubectl(
            kubeconfig, "auth", "can-i", verb, resource, "-n", namespace,
            "--as=" + identity,
        )
        answer = result.stdout.strip()
        if answer != ("yes" if allowed else "no") or result.returncode != (0 if allowed else 1):
            raise RuntimeError(
                f"RBAC {identity} {verb} {resource} in {namespace}: "
                f"expected {'yes' if allowed else 'no'}, got {answer!r}; {result.stderr.strip()}"
            )
        print(f"PASS RBAC {identity} {verb} {resource} in {namespace}: {answer}")


def check_quota(kubeconfig):
    result = require_success(
        kubectl(kubeconfig, "get", "resourcequota", "spawnpod-namespace-budget", "-n", "ci", "-o", "json"),
        "read ConfigMap quota",
    )
    quota = json.loads(result.stdout)
    hard = int(quota["status"]["hard"]["count/configmaps"])
    used = int(quota["status"]["used"]["count/configmaps"])
    headroom = hard - used
    if headroom < 0:
        raise RuntimeError(f"quota headroom is negative: {headroom}")
    if headroom > 8:
        check_temporary_quota(kubeconfig)
        return
    prefix = "spawnpod-quota-check-" + uuid.uuid4().hex[:8]
    created = []
    try:
        for index in range(headroom):
            name = f"{prefix}-{index}"
            require_success(kubectl(kubeconfig, "create", "-f", "-", obj=configmap(name)), f"create {name}")
            created.append(name)
        expect_denied(kubeconfig, configmap(f"{prefix}-denied"), "exceeded quota", "ConfigMap quota")
    finally:
        for name in reversed(created):
            require_success(kubectl(kubeconfig, "delete", "configmap", name, "-n", "ci", "--wait=true"), f"delete {name}")
    print(f"PASS quota cleanup: removed {len(created)} temporary ConfigMaps")


def check_temporary_quota(kubeconfig):
    name = "spawnpod-quota-check-" + uuid.uuid4().hex[:8]
    quota = {
        "apiVersion": "v1",
        "kind": "ResourceQuota",
        "metadata": {"name": name, "namespace": "ci", "labels": {"spawnpod.dev/smoke": "quota"}},
        "spec": {"hard": {"count/configmaps": "0"}},
    }
    require_success(kubectl(kubeconfig, "create", "-f", "-", obj=quota), "create temporary ResourceQuota")
    try:
        for _ in range(20):
            result = require_success(kubectl(kubeconfig, "get", "resourcequota", name, "-n", "ci", "-o", "json"), "read temporary ResourceQuota")
            if json.loads(result.stdout).get("status", {}).get("hard", {}).get("count/configmaps") == "0":
                break
            time.sleep(0.25)
        else:
            raise RuntimeError("temporary ResourceQuota did not become active")
        expect_denied(kubeconfig, configmap(name + "-denied"), "exceeded quota", "ConfigMap quota")
    finally:
        require_success(kubectl(kubeconfig, "delete", "resourcequota", name, "-n", "ci", "--wait=true"), "delete temporary ResourceQuota")
    print("PASS quota cleanup: removed temporary ResourceQuota")


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--kubeconfig", required=True, type=Path)
    parser.add_argument("--config", required=True, type=Path)
    args = parser.parse_args()
    config = args.config.read_text()
    agent_image = config_image(config, r"^agentImage:\s*(\S+)")
    job_image = config_image(config, r"^\s+image:\s*(\S+)")

    baseline = pod("spawnpod-policy-image-check", agent_image, job_image)
    require_success(
        kubectl(args.kubeconfig, "create", "--dry-run=server", "-f", "-", obj=baseline),
        "valid Pod server dry-run",
    )
    print("PASS approved Pod shape: server dry-run admitted")

    wrong_image = pod("spawnpod-policy-image-denied", agent_image, job_image)
    wrong_image["spec"]["containers"][0]["image"] = "docker.io/library/alpine:latest"
    expect_denied(args.kubeconfig, wrong_image, "Pod images differ", "unapproved image")

    privileged = pod("spawnpod-policy-privileged-denied", agent_image, job_image)
    privileged["spec"]["containers"][0]["securityContext"]["privileged"] = True
    privileged["spec"]["containers"][0]["securityContext"]["allowPrivilegeEscalation"] = True
    expect_denied(args.kubeconfig, privileged, "violates PodSecurity", "privileged container")

    host_network = pod("spawnpod-policy-host-network-denied", agent_image, job_image)
    host_network["spec"]["hostNetwork"] = True
    expect_denied(args.kubeconfig, host_network, "violates PodSecurity", "host network")

    host_path = pod("spawnpod-policy-host-path-denied", agent_image, job_image)
    host_path["spec"]["volumes"].append({"name": "host", "hostPath": {"path": "/etc"}})
    expect_denied(args.kubeconfig, host_path, "violates PodSecurity", "hostPath volume")

    secret_volume = pod("spawnpod-policy-secret-volume-denied", agent_image, job_image)
    secret_volume["spec"]["volumes"].append({"name": "secret", "secret": {"secretName": "probe"}})
    expect_denied(args.kubeconfig, secret_volume, "only bounded emptyDir volumes", "Secret volume")

    secret_env = pod("spawnpod-policy-secret-env-denied", agent_image, job_image)
    secret_env["spec"]["containers"][0]["env"] = [{
        "name": "PROBE", "valueFrom": {"secretKeyRef": {"name": "probe", "key": "value"}}
    }]
    expect_denied(args.kubeconfig, secret_env, "PodSpec environment and host ports are forbidden", "Secret environment variable")

    host_port = pod("spawnpod-policy-host-port-denied", agent_image, job_image)
    host_port["spec"]["containers"][0]["ports"] = [{"containerPort": 8080, "hostPort": 18080}]
    expect_denied(args.kubeconfig, host_port, "violates PodSecurity", "host port")

    check_rbac(args.kubeconfig)
    check_quota(args.kubeconfig)


if __name__ == "__main__":
    main()
