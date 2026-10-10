#!/usr/bin/env python3
"""Write a private local Runner config from Forgejo offline registration."""

import argparse
import os
import re
import uuid
from pathlib import Path


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--template", type=Path, required=True)
    parser.add_argument("--uuid-file", type=Path, required=True)
    parser.add_argument("--secret-file", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()

    runner_uuid = str(uuid.UUID(args.uuid_file.read_text().strip()))
    secret = args.secret_file.read_text().strip()
    if not re.fullmatch(r"[0-9a-f]{40}", secret):
        raise SystemExit("Runner shared secret must be 40 hexadecimal characters")

    template = args.template.read_text()
    marker = "  connections: {}"
    if template.count(marker) != 1:
        raise SystemExit("Runner template must have one empty connections map")
    config = template.replace(
        marker,
        "  connections:\n"
        "    forgejo:\n"
        "      url: http://127.0.0.1:3000/\n"
        f"      uuid: {runner_uuid}\n"
        f"      token: {secret}",
    )
    fd = os.open(args.output, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, "w") as output:
        output.write(config)
    os.chmod(args.output, 0o600)
    print(f"Wrote private Runner config to {args.output}")


if __name__ == "__main__":
    main()
