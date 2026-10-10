#!/usr/bin/env python3
"""Enable one approved Alpine echo service in the private local smoke config."""

import argparse
import re
from pathlib import Path


def replace_once(text, old, new):
    if text.count(old) != 1:
        raise SystemExit(f"expected exactly one config fragment: {old!r}")
    return text.replace(old, new, 1)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--config", required=True, type=Path)
    args = parser.parse_args()
    source = args.config.read_text()
    if "    services:\n      echo:\n" in source:
        if "enableServices: true" not in source or "maxNetworkPolicies: 3" not in source:
            raise SystemExit("existing echo service does not match expected smoke gate and quota")
    else:
        match = re.search(r"^    image: (docker\.io/library/alpine@sha256:[0-9a-f]{64})$", source, re.MULTILINE)
        if not match:
            raise SystemExit("expected one pinned Alpine job image")
        image = match.group(1)
        source = replace_once(source, "    maxNetworkPolicies: 1\n", "    maxNetworkPolicies: 3\n")
        source = replace_once(source, "  enableServices: false\n", "  enableServices: true\n")
        service = f"""    services:
      echo:
        image: {image}
        command: ["/bin/busybox", "nc", "-lk", "-p", "8080", "-e", "/bin/cat"]
        architecture: amd64
        uid: 10001
        gid: 10001
        port: 8080
        cpuRequest: 100m
        cpuLimit: 250m
        memoryRequest: 128Mi
        memoryLimit: 256Mi
        storageRequest: 512Mi
        storageLimit: 1Gi
"""
        source = replace_once(source, "    maxLifetime: 1h\n", "    maxLifetime: 1h\n" + service)

    # Two jobs and one service can coexist for cross-environment isolation.
    for old, new in {
        "    maxPods: 2\n": "    maxPods: 3\n",
        "    requestsMemory: 512Mi\n": "    requestsMemory: 1Gi\n",
        "    limitsMemory: 2Gi\n": "    limitsMemory: 3Gi\n",
        "    requestsStorage: 2Gi\n": "    requestsStorage: 3Gi\n",
        "    limitsStorage: 8Gi\n": "    limitsStorage: 12Gi\n",
        "    limitsCPU: \"2\"\n": "    limitsCPU: \"3\"\n",
    }.items():
        if old in source:
            source = replace_once(source, old, new)
        elif new not in source:
            raise SystemExit(f"missing expected quota field: {old.strip()}")
    args.config.write_text(source)
    print("Enabled local echo service; render and apply admission, then restart controller")


if __name__ == "__main__":
    main()
