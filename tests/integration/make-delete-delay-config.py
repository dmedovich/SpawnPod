#!/usr/bin/env python3
"""Make a private local config with a short cleanup timeout for failure tests."""

import os
from pathlib import Path


def main() -> None:
    source = Path(".local/config.yaml")
    output_path = Path(".local/config.delete-fail.yaml")
    data = source.read_text()
    old = "cleanupTimeout: 2m"
    if data.count(old) != 1:
        raise ValueError("expected one 2m cleanup timeout in local config")
    fd = os.open(output_path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, "w") as output:
        os.fchmod(output.fileno(), 0o600)
        output.write(data.replace(old, "cleanupTimeout: 8s"))


if __name__ == "__main__":
    main()
