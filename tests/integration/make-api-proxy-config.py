#!/usr/bin/env python3
"""Make private local configs that route only the test controller through a TCP proxy."""

import os
from pathlib import Path


def replace_once(source: Path, old: str, new: str) -> str:
    data = source.read_text()
    if data.count(old) != 1:
        raise ValueError(f"expected one replacement target in {source}")
    return data.replace(old, new)


def write_private(path: Path, data: str) -> None:
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, "w") as output:
        os.fchmod(output.fileno(), 0o600)
        output.write(data)


def main() -> None:
    kubeconfig = Path(".local/kubeconfig").resolve()
    proxy_kubeconfig = Path(".local/kubeconfig-api-proxy").resolve()
    config = Path(".local/config.yaml").resolve()
    proxy_config = Path(".local/config.api-proxy.yaml").resolve()
    if not kubeconfig.is_file() or not config.is_file():
        raise ValueError("prepare .local/kubeconfig and .local/config.yaml first")
    kubeconfig_data = replace_once(
        kubeconfig, "server: https://127.0.0.1:744", "server: https://127.0.0.1:1744"
    )
    config_data = replace_once(config, str(kubeconfig), str(proxy_kubeconfig))
    write_private(proxy_kubeconfig, kubeconfig_data)
    write_private(proxy_config, config_data)


if __name__ == "__main__":
    main()
