#!/usr/bin/env python3
"""Create the local smoke repository and run its Forgejo workflow."""

import argparse
import base64
import json
import http.cookiejar
import posixpath
import re
import secrets
import signal
import time
import urllib.error
import urllib.request
import urllib.parse
from pathlib import Path


BASE = "http://127.0.0.1:3000/api/v1"
OWNER = "spawnpod-admin"
REPO = "spawnpod-smoke"


def api(token, method, path, body=None, sensitive=False, authorization=None):
    payload = None if body is None else json.dumps(body).encode()
    request = urllib.request.Request(
        BASE + path,
        data=payload,
        method=method,
        headers={
            "Authorization": authorization or "token " + token,
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
        detail = 'private operation failed' if sensitive else data[:400]
        raise RuntimeError(f"{method} {path}: HTTP {error.code}: {detail}") from error


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


def cancel_run(token, run_id):
    """Forgejo v15 exposes cancellation through its origin-protected web UI.

    A disposable repository writer authenticates exactly as a browser would.
    Its password/session remain in memory, and it never enters a job.
    """
    _, run = api(token, 'GET', f'/repos/{OWNER}/{REPO}/actions/runs/{run_id}')
    index = run['index_in_repo']
    if not isinstance(index, int) or index <= 0:
        raise RuntimeError('invalid owned run index')
    username = 'rc7-cancel-' + secrets.token_hex(12)
    password = secrets.token_urlsafe(32)
    created = False
    try:
        api(token, 'POST', '/admin/users', {'username': username, 'email': username+'@localhost.invalid', 'password': password, 'must_change_password': False, 'send_notify': False}, sensitive=True)
        created = True
        api(token, 'PUT', f'/repos/{OWNER}/{REPO}/collaborators/{username}', {'permission': 'write'}, sensitive=True)
        origin = BASE.removesuffix('/api/v1')
        opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))
        # Forgejo v15 uses Go CrossOriginProtection, not a hidden CSRF token.
        headers = {'Content-Type': 'application/x-www-form-urlencoded', 'Origin': origin, 'Sec-Fetch-Site': 'same-origin'}
        payload = urllib.parse.urlencode({'user_name': username, 'password': password}).encode()
        with opener.open(urllib.request.Request(origin+'/user/login', data=payload, headers=headers), timeout=15) as response:
            if '/user/login' in response.url:
                raise RuntimeError('disposable writer login failed')
        path = f'/{OWNER}/{REPO}/actions/runs/{index}/cancel'
        request = urllib.request.Request(origin+path, data=b'{}', headers={**headers, 'Content-Type':'application/json'}, method='POST')
        with opener.open(request, timeout=15) as response:
            if response.status != 200 or json.load(response) != {}:
                raise RuntimeError('Forgejo UI did not acknowledge cancellation')
    finally:
        if created:
            api(token, 'DELETE', f'/admin/users/{username}?purge=true', sensitive=True)
            try:
                api(token, 'GET', f'/users/{username}', sensitive=True)
            except RuntimeError as error:
                if 'HTTP 404' not in str(error):
                    raise
            else:
                raise RuntimeError('temporary cancellation writer still exists')
            print('Disposable cancellation writer removed; absence verified', flush=True)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--token-file", type=Path, required=True)
    parser.add_argument("--workflow", type=Path, required=True)
    parser.add_argument("--asset", action="append", default=[], metavar="LOCAL=REPO_PATH")
    parser.add_argument("--input", action="append", default=[], metavar="NAME=VALUE")
    parser.add_argument("--package-secret", help="create a disposable write:package token and unique RC7 Actions secret; remove both afterwards")
    parser.add_argument("--cancel-trigger", type=Path, help="cancel this run once the caller creates this file")
    parser.add_argument("--expect-cancelled", action="store_true")
    args = parser.parse_args()
    if bool(args.cancel_trigger) != args.expect_cancelled:
        parser.error("cancellation requires both --cancel-trigger and --expect-cancelled")
    if args.cancel_trigger and args.cancel_trigger.exists():
        parser.error("cancel trigger must not exist before dispatch")
    if args.package_secret and not re.fullmatch(r'RC7_OCI_[A-F0-9]{32}', args.package_secret):
        parser.error('package secret must be a unique RC7 fixture name')
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

    package_token = None
    package_owner = None
    package_authorization = None
    secret_created = False
    try:
        if args.package_secret:
            owner = 'rc7-oci-' + args.package_secret[-16:].lower()
            password = secrets.token_urlsafe(32)
            api(token, 'POST', '/admin/users', {'username': owner, 'email': owner+'@localhost.invalid', 'password': password, 'must_change_password': False, 'send_notify': False}, sensitive=True)
            package_owner = owner
            package_authorization = 'Basic ' + base64.b64encode((owner+':'+password).encode()).decode()
            _, package_token = api(token, "POST", f"/users/{owner}/tokens", {"name": "rc7-package", "scopes": ["write:package"]}, sensitive=True, authorization=package_authorization)
            _, existing_secrets = api(token, 'GET', f'/repos/{OWNER}/{REPO}/actions/secrets')
            if any(item['name'] == args.package_secret for item in existing_secrets):
                raise RuntimeError('refusing to overwrite an existing Actions secret')
            status, _ = api(token, "PUT", f"/repos/{OWNER}/{REPO}/actions/secrets/{args.package_secret}", {"data": package_token["sha1"]}, sensitive=True)
            if status != 201:
                raise RuntimeError("disposable secret unexpectedly existed")
            secret_created = True
        dispatch(args, token, assets, workflow_path, inputs)
    finally:
        cleanup_errors = []
        if secret_created:
            try:
                api(token, "DELETE", f"/repos/{OWNER}/{REPO}/actions/secrets/{args.package_secret}", sensitive=True)
                _, remaining = api(token, 'GET', f'/repos/{OWNER}/{REPO}/actions/secrets', sensitive=True)
                if any(item['name'] == args.package_secret for item in remaining):
                    raise RuntimeError('secret still exists')
            except RuntimeError:
                cleanup_errors.append("Actions secret cleanup failed")
        if package_token:
            try:
                api(token, "DELETE", f"/users/{package_owner}/tokens/{package_token['id']}", sensitive=True, authorization=package_authorization)
                _, remaining = api(token, 'GET', f'/users/{package_owner}/tokens', sensitive=True, authorization=package_authorization)
                if any(item['id'] == package_token['id'] for item in remaining):
                    raise RuntimeError('token still exists')
            except RuntimeError:
                cleanup_errors.append("package token cleanup failed")
        if package_owner:
            try:
                api(token, 'DELETE', f'/admin/users/{package_owner}?purge=true', sensitive=True)
                try:
                    api(token, 'GET', f'/users/{package_owner}', sensitive=True)
                except RuntimeError as error:
                    if 'HTTP 404' not in str(error):
                        raise
                else:
                    raise RuntimeError('package owner still exists')
            except RuntimeError:
                cleanup_errors.append('disposable package owner cleanup failed')
        if cleanup_errors:
            raise RuntimeError("; ".join(cleanup_errors))
        if package_owner:
            print("Disposable Actions secret, package token and package owner cleaned up; absence verified", flush=True)


def dispatch(args, token, assets, workflow_path, inputs):
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
    if not run_id:
        raise RuntimeError("dispatch did not return an owned run ID")
    deadline = time.monotonic() + 10 * 60
    cancel_sent = False
    terminal = False
    try:
        while time.monotonic() < deadline:
            if args.cancel_trigger and args.cancel_trigger.exists() and not cancel_sent:
                cancel_run(token, run_id)
                cancel_sent = True
                print('Requested cancellation of the executing source workflow', flush=True)
            _, current = api(token, "GET", f"/repos/{OWNER}/{REPO}/actions/runs/{run_id}")
            if current:
                status = current.get("status")
                conclusion = current.get("conclusion")
                print(f"run {current['id']}: {status}, {conclusion}", flush=True)
                if status in ("success", "failure", "cancelled", "skipped"):
                    terminal = True
                    if status == "cancelled" and args.expect_cancelled and cancel_sent:
                        return
                    if status != "success" or args.expect_cancelled:
                        raise SystemExit(f"workflow finished with status {status}")
                    return
                if status == "completed":
                    terminal = True
                    if conclusion == "cancelled" and args.expect_cancelled and cancel_sent:
                        return
                    if conclusion != "success" or args.expect_cancelled:
                        raise SystemExit("workflow did not succeed")
                    return
            time.sleep(3)
        raise SystemExit("workflow did not complete within 10 minutes; cancellation requested")
    finally:
        if not terminal and not cancel_sent:
            cancel_run(token, run_id)
            print('Interrupted workflow: cancellation requested', flush=True)



def interrupted(signum, frame):
    raise KeyboardInterrupt("termination requested")


if __name__ == "__main__":
    signal.signal(signal.SIGTERM, interrupted)
    main()
