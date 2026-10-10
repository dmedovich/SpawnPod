#!/usr/bin/env python3
"""Complete the local Forgejo install form without printing credentials."""

import argparse
import http.cookiejar
import os
import secrets
import urllib.parse
import urllib.request
from html.parser import HTMLParser
from pathlib import Path


class InstallForm(HTMLParser):
    def __init__(self):
        super().__init__()
        self.in_form = False
        self.fields = {}

    def handle_starttag(self, tag, attrs):
        attributes = dict(attrs)
        if tag == "form" and attributes.get("method", "").lower() == "post":
            self.in_form = True
        if not self.in_form or tag != "input" or "name" not in attributes:
            return
        if attributes.get("type") == "checkbox" and "checked" not in attributes:
            return
        self.fields[attributes["name"]] = attributes.get("value", "on")

    def handle_endtag(self, tag):
        if tag == "form":
            self.in_form = False


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--url", default="http://127.0.0.1:3000/")
    parser.add_argument("--password-file", required=True, type=Path)
    args = parser.parse_args()

    opener = urllib.request.build_opener(
        urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar())
    )
    with opener.open(args.url, timeout=15) as response:
        form = InstallForm()
        form.feed(response.read().decode("utf-8"))
    if form.fields.get("db_type") != "sqlite3" or "admin_name" not in form.fields:
        raise SystemExit("Forgejo install form is not available")

    args.password_file.parent.mkdir(parents=True, exist_ok=True)
    if args.password_file.exists():
        password = args.password_file.read_text().strip()
    else:
        password = secrets.token_urlsafe(36)
        fd = os.open(args.password_file, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(fd, "w") as output:
            output.write(password + "\n")

    fields = form.fields
    fields.update(
        admin_name="spawnpod-admin",
        admin_email="spawnpod-admin@localhost.invalid",
        admin_passwd=password,
        admin_confirm_passwd=password,
        domain="127.0.0.1",
        app_url=args.url,
        disable_registration="on",
        offline_mode="on",
    )
    fields.pop("enable_update_checker", None)
    request = urllib.request.Request(
        args.url,
        data=urllib.parse.urlencode(fields).encode(),
        headers={"Content-Type": "application/x-www-form-urlencoded"},
    )
    with opener.open(request, timeout=120) as response:
        final_url = response.url
        body = response.read().decode("utf-8", errors="replace")
    if "name=\"admin_name\"" in body:
        raise SystemExit("Forgejo returned the install form; inspect its container logs")
    print(f"Forgejo install submitted; final URL: {final_url}")
    print(f"Admin: spawnpod-admin; password file: {args.password_file}")


if __name__ == "__main__":
    main()
