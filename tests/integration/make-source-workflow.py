#!/usr/bin/env python3
"""Generate a local two-label Runner workflow with real network assertions."""
import argparse
import ipaddress
from pathlib import Path
import re
import shlex
from urllib.parse import urlparse

import yaml


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--proxy', required=True)
    parser.add_argument('--upstream-host', required=True)
    parser.add_argument('--upstream-ip', required=True)
    parser.add_argument('--output', required=True, type=Path)
    parser.add_argument('--ca-file', required=True, type=Path)
    parser.add_argument('--package-name', required=True)
    parser.add_argument('--secret-name', required=True)
    args = parser.parse_args()
    endpoint = urlparse(args.proxy)
    if endpoint.scheme != 'http' or endpoint.username or endpoint.password or endpoint.path or endpoint.query or endpoint.fragment or not endpoint.port:
        parser.error('proxy must be a plain HTTP IP:port endpoint')
    ipaddress.ip_address(endpoint.hostname)
    ipaddress.ip_address(args.upstream_ip)
    if not re.fullmatch(r'[a-z0-9]+(?:[.-][a-z0-9]+)*', args.upstream_host):
        parser.error('invalid upstream host')
    if not re.fullmatch(r'rc7-transport-[a-f0-9]{32}', args.package_name) or not re.fullmatch(r'RC7_OCI_[A-F0-9]{32}', args.secret_name):
        parser.error('transport package and secret must have unique RC7 fixture names')
    probe = (Path(__file__).with_name('source-network-probe.js')).read_text().replace('process.argv.slice(2)', 'process.argv.slice(1)')

    def check(mode, address, port):
        return shlex.join(['node', '-e', probe, mode, str(address), str(port), args.upstream_host])

    isolated = ['set -eu', 'test -z "${HTTP_PROXY:-}"', 'test -z "${HTTPS_PROXY:-}"',
                check('tcp-deny', endpoint.hostname, endpoint.port), check('tcp-deny', args.upstream_ip, 443), check('tcp-deny', '10.43.0.1', 443)]
    source = ['set -eu', 'test "$HTTP_PROXY" = ' + shlex.quote(args.proxy),
              'test "$HTTPS_PROXY" = "$HTTP_PROXY"', 'test "$http_proxy" = "$HTTP_PROXY"', 'test "$https_proxy" = "$HTTP_PROXY"',
              'test -z "${NO_PROXY:-}"', 'test -z "${no_proxy:-}"']
    source += [check(mode, endpoint.hostname, endpoint.port) for mode in ('https-proxy', 'host-deny', 'port-deny', 'sni-deny')]
    source += [check('tcp-deny', args.upstream_ip, 443), check('tcp-deny', '10.43.0.1', 443)]
    jobs = {}
    for name, label, commands in (('isolated', 'spawnpod', isolated), ('source', 'spawnpod-source', source)):
        jobs[name] = {'runs-on': label, 'timeout-minutes': 10, 'defaults': {'run': {'shell': 'sh', 'working-directory': '/workspace'}},
                      'steps': [{'name': 'Verify profile and enforced network boundary', 'run': '\n'.join(commands) + '\n'}]}
    cert = args.ca_file.read_text()
    jobs['source']['steps'] += [
        {'name': 'Trust only the disposable Forgejo TLS fixture', 'run': 'set -eu\nprintf %s ' + shlex.quote(cert) + ' > /workspace/rc7-ca.pem\ngit --version\n'},
        {'name': 'Checkout private Forgejo repository through the proxy', 'uses': 'actions/checkout@11d5960a326750d5838078e36cf38b85af677262',
         'env': {'NODE_EXTRA_CA_CERTS': '/workspace/rc7-ca.pem', 'GIT_SSL_CAINFO': '/workspace/rc7-ca.pem'},
         'with': {'github-server-url': 'https://forgejo.rc7.test:3443', 'repository': 'spawnpod-admin/spawnpod-smoke', 'ref': '${{ github.sha }}', 'fetch-depth': 1, 'persist-credentials': False}},
        {'name': 'Verify checkout commit, shallow history and origin', 'run': 'set -eu\ntest "$(git -C "$GITHUB_WORKSPACE" rev-parse HEAD)" = "$GITHUB_SHA"\ntest "$(git -C "$GITHUB_WORKSPACE" rev-parse --is-shallow-repository)" = true\ntest "$(git -C "$GITHUB_WORKSPACE" remote get-url origin)" = https://forgejo.rc7.test:3443/spawnpod-admin/spawnpod-smoke\n'},
    ]
    client = Path(__file__).with_name('source-oci-client.js').read_text()
    jobs['source']['steps'].append({'name': 'Push, pull and remove a scoped OCI artifact through HTTPS_PROXY',
        'env': {'RC7_OCI_TOKEN': '${{ secrets.' + args.secret_name + ' }}', 'RC7_OCI_OWNER': 'rc7-oci-' + args.secret_name[-16:].lower(), 'RC7_PACKAGE': args.package_name, 'RC7_CA_FILE': '/workspace/rc7-ca.pem'},
        'run': 'set -eu\n' + shlex.join(['node', '-e', client]) + '\nif HTTPS_PROXY=http://127.0.0.1:1 ' + shlex.join(['node', '-e', client]) + '; then\n  echo "OCI client bypassed unavailable proxy"\n  exit 1\nfi\necho "Unavailable proxy correctly fails OCI transport"\n'})
    args.output.write_text(yaml.safe_dump({'name': 'SpawnPod RC7 two profiles', 'on': 'workflow_dispatch', 'jobs': jobs}, sort_keys=False))


if __name__ == '__main__':
    main()
