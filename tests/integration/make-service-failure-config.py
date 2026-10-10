#!/usr/bin/env python3
"""Make a private local config that fails when starting the approved service."""

import argparse
import os
from pathlib import Path


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--config", type=Path, default=Path(".local/config.yaml"))
    parser.add_argument("--output", type=Path, default=Path(".local/config.service-fail.yaml"))
    args = parser.parse_args()

    if args.config.resolve() == args.output.resolve():
        parser.error("output must differ from config")
    original = args.config.read_text()
    command = 'command: ["/bin/busybox", "nc", "-lk", "-p", "8080", "-e", "/bin/cat"]'
    if original.count(command) != 1:
        parser.error("expected exactly one approved echo service command")
    test_config = original.replace(command, 'command: ["/missing-service-binary"]')
    fd = os.open(args.output, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, "w") as output:
        os.fchmod(output.fileno(), 0o600)
        output.write(test_config)


if __name__ == "__main__":
    main()
