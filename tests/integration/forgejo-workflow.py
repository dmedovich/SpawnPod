#!/usr/bin/env python3
"""Create the local smoke repository and run its Forgejo workflow."""

import argparse
import base64
import json
import posixpath
import re
import time
import urllib.error
import urllib.request
from pathlib import Path


BASE = "http://127.0.0.1:3000/api/v1"
OWNER = "spawnpod-admin"
REPO = "spawnpod-smoke"


def api(token, method, path, body=None):
    payload = None if body is None else json.dumps(body).encode()
    request = urllib.request.Request(
        BASE + path,
        data=payload,
        method=method,
        headers={
            "Authorization": "token " + token,
            "Accept": "application/json",
            "Content-Type": "application/json",
        },
    )
    try:
        with urllib.request.urlopen(request, timeout=30) as response:
            data = response.read()
            return response.status, json.loads(data) if data else None
    except urllib.error.HTTPError as error:
        data = error.read().decode(errors="replace")
        raise RuntimeError(f"{method} {path}: HTTP {error.code}: {data[:400]}") from error


def upload(token, source, target):
    encoded = base64.b64encode(source.read_bytes()).decode()
    endpoint = f"/repos/{OWNER}/{REPO}/contents/{target}"
    try:
        _, existing = api(token, "GET", endpoint)
        if existing["content"].replace("\n", "") == encoded:
            return
        api(token, "PUT", endpoint, {
            "content": encoded, "sha": existing["sha"],
            "message": "Update SpawnPod smoke file", "branch": "main",
        })
        print(f"Updated {target}", flush=True)
    except RuntimeError as error:
        if "HTTP 404" not in str(error):
            raise
        api(token, "POST", endpoint, {
            "content": encoded, "message": "Add SpawnPod smoke file", "branch": "main",
        })
        print(f"Committed {target}", flush=True)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--token-file", type=Path, required=True)
    parser.add_argument("--workflow", type=Path, required=True)
    parser.add_argument("--asset", action="append", default=[], metavar="LOCAL=REPO_PATH")
    parser.add_argument("--input", action="append", default=[], metavar="NAME=VALUE")
    args = parser.parse_args()
    if args.workflow.suffix not in (".yml", ".yaml"):
        raise SystemExit("workflow must be a .yml or .yaml file")
    workflow_path = ".forgejo/workflows/" + args.workflow.name
    assets = []
    for item in args.asset:
        local, separator, target = item.partition("=")
        if (separator != "=" or not local or not target.startswith(".forgejo/actions/")
                or posixpath.normpath(target) != target or target.endswith("/")):
            parser.error("--asset must be LOCAL=.forgejo/actions/PATH")
        assets.append((Path(local), target))
    inputs = {}
    for item in args.input:
        name, separator, value = item.partition("=")
        if separator != "=" or not re.fullmatch(r"[A-Za-z][A-Za-z0-9_-]{0,63}", name):
            parser.error("--input must be NAME=VALUE with a valid input name")
        if name in inputs:
            parser.error("duplicate workflow input name")
        inputs[name] = value
    token = args.token_file.read_text().strip()
    if not token:
        raise SystemExit("empty Forgejo API token")

    _, user = api(token, "GET", "/user")
    if user["login"] != OWNER:
        raise SystemExit("API token belongs to an unexpected user")
    print(f"Forgejo API authenticated as {OWNER}", flush=True)

    try:
        api(token, "GET", f"/repos/{OWNER}/{REPO}")
    except RuntimeError as error:
        if "HTTP 404" not in str(error):
            raise
        api(
            token,
            "POST",
            "/user/repos",
            {"name": REPO, "private": True, "auto_init": True, "default_branch": "main"},
        )
        print(f"Created repository {OWNER}/{REPO}", flush=True)

    for source, target in assets:
        upload(token, source, target)
    upload(token, args.workflow, workflow_path)

    dispatch = {"ref": "main", "return_run_info": True}
    if inputs:
        dispatch["inputs"] = inputs
    _, run = api(
        token,
        "POST",
        f"/repos/{OWNER}/{REPO}/actions/workflows/{args.workflow.name}/dispatches",
        dispatch,
    )
    run_id = run.get("id") if isinstance(run, dict) else None
    print(f"Dispatched workflow, run ID: {run_id}", flush=True)
    deadline = time.monotonic() + 10 * 60
    while time.monotonic() < deadline:
        if run_id:
            _, current = api(token, "GET", f"/repos/{OWNER}/{REPO}/actions/runs/{run_id}")
        else:
            _, listing = api(token, "GET", f"/repos/{OWNER}/{REPO}/actions/runs?limit=1")
            current = listing["workflow_runs"][0] if listing.get("workflow_runs") else None
        if current:
            status = current.get("status")
            conclusion = current.get("conclusion")
            print(f"run {current['id']}: {status}, {conclusion}", flush=True)
            if status in ("success", "failure", "cancelled", "skipped"):
                if status != "success":
                    raise SystemExit(f"workflow finished with status {status}")
                return
            if status == "completed":
                if conclusion != "success":
                    raise SystemExit("workflow did not succeed")
                return
        time.sleep(3)
    raise SystemExit("workflow did not complete within 10 minutes")


if __name__ == "__main__":
    main()
