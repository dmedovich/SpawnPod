#!/usr/bin/env python3
"""Opt-in CNI gate in new namespaces; never modifies cluster-wide admission.

This tests rendered job policies and the real proxy Deployment. Probe Pods are
Restricted, tokenless fixtures, not SpawnPod/Runner execution acceptance.
"""
import argparse
import ipaddress
import json
import os
from pathlib import Path
import re
import subprocess
import time
import urllib.parse
import uuid

import yaml


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--kubeconfig', required=True, type=Path)
    parser.add_argument('--spawnpod-binary', required=True, type=Path)
    parser.add_argument('--runtime-image', required=True)
    parser.add_argument('--job-image', required=True, help='digest-pinned non-root image containing Node and CA trust')
    parser.add_argument('--upstream-host', required=True)
    parser.add_argument('--upstream-ip', required=True)
    parser.add_argument('--evidence-dir', required=True, type=Path)
    parser.add_argument('--diagnose-dnat', action='store_true', help='on Service failure, test an exact backend IP in disposable namespaces; never counts as acceptance')
    parser.add_argument('--policy-mode', choices=('ip', 'pod-selector'), default='pod-selector')
    args = parser.parse_args()
    root = Path(__file__).resolve().parents[2]
    for image in (args.runtime_image, args.job_image):
        if not re.fullmatch(r'[^\s]+@sha256:[a-f0-9]{64}', image):
            parser.error('images must be pinned by digest')
    if not re.fullmatch(r'[a-z0-9]+(?:[.-][a-z0-9]+)*', args.upstream_host):
        parser.error('upstream host must be a lowercase DNS name')
    upstream = ipaddress.ip_address(args.upstream_ip)
    if not upstream.is_global or upstream.version != 4:
        parser.error('this gate requires an operator-verified public IPv4 upstream')
    os.umask(0o077)
    evidence = args.evidence_dir.resolve()
    evidence.mkdir(parents=True, exist_ok=False)
    kubeconfig = args.kubeconfig.resolve()
    binary = args.spawnpod_binary.resolve()
    version = subprocess.run([str(binary), 'version'], check=True, capture_output=True, text=True, timeout=15).stdout.strip()
    if version != 'dev' and not re.fullmatch(r'v[0-9]+\.[0-9]+\.[0-9]+(?:-[A-Za-z0-9.-]+)?', version):
        parser.error('unrecognized CLI version')
    kubectl = ['kubectl', '--kubeconfig', str(kubeconfig), '--request-timeout=15s']

    def k(*arguments, obj=None, timeout=45):
        result = subprocess.run(kubectl + list(arguments), input=None if obj is None else json.dumps(obj), text=True, capture_output=True, timeout=timeout)
        if result.returncode:
            raise RuntimeError(f'kubectl {arguments}: {result.stderr.strip()}')
        return result.stdout

    def get(kind, name=None, namespace=None):
        arguments = ['get', kind]
        if name:
            arguments.append(name)
        if namespace:
            arguments += ['-n', namespace]
        return json.loads(k(*arguments, '-o', 'json'))

    endpoint = json.loads(k('config', 'view', '--minify', '-o', 'json'))['clusters'][0]['cluster']['server']
    if not ipaddress.ip_address(urllib.parse.urlparse(endpoint).hostname).is_loopback:
        parser.error('this disposable local gate refuses non-loopback clusters')
    identifier = uuid.uuid4().hex[:10]
    workload, infra, unauthorized = [f'sp-rc7-net-{role}-{identifier}' for role in ('ci', 'infra', 'other')]
    created = []
    results = []

    def save(name, value):
        (evidence / name).write_text(json.dumps(value, indent=2) + '\n')

    def create(obj):
        return json.loads(k('create', '-f', '-', '-o', 'json', obj=obj))

    def probe_pod(name, namespace, labels):
        security = {'runAsNonRoot': True, 'runAsUser': 10001, 'runAsGroup': 10001, 'seccompProfile': {'type': 'RuntimeDefault'}}
        container_security = dict(security, readOnlyRootFilesystem=True, allowPrivilegeEscalation=False, capabilities={'drop': ['ALL']})
        return create({'apiVersion': 'v1', 'kind': 'Pod', 'metadata': {'name': name, 'namespace': namespace, 'labels': labels}, 'spec': {
            'automountServiceAccountToken': False, 'enableServiceLinks': False, 'restartPolicy': 'Never', 'securityContext': security,
            'containers': [{'name': 'probe', 'image': args.job_image, 'imagePullPolicy': 'IfNotPresent', 'command': ['node', '-e', "for (const port of [8080,3128]) require('http').createServer((q,r)=>r.end('canary')).listen(port,'0.0.0.0')"], 'securityContext': container_security,
                            'resources': {'requests': {'cpu': '10m', 'memory': '32Mi'}, 'limits': {'cpu': '250m', 'memory': '128Mi'}}}]}})

    script = (root / 'tests/integration/source-network-probe.js').read_text()

    def check(pod, mode, address, port, host=args.upstream_host):
        command = kubectl + ['exec', '-n', pod['metadata']['namespace'], pod['metadata']['name'], '--', 'node', '-e', script.replace('process.argv.slice(2)', 'process.argv.slice(1)'), mode, str(address), str(port), host]
        execution = subprocess.run(command, text=True, capture_output=True, timeout=25)
        try:
            result = json.loads(execution.stdout)
        except ValueError:
            raise RuntimeError(f'probe did not return JSON: {execution.stderr.strip()}') from None
        result['pod'] = pod['metadata']['name']
        result['address'], result['port'] = address, port
        results.append(result)
        save('results.json', results)
        print(f"{result['pod']}: {mode}: {result['detail']}", flush=True)
        if execution.returncode or not result['ok']:
            raise RuntimeError('network assertion failed')

    try:
        save('nodes.json', get('nodes'))
        save('version.json', json.loads(k('version', '-o', 'json')))
        save('system-pods.json', get('pods', namespace='kube-system'))
        for namespace in (workload, infra, unauthorized):
            obj = create({'apiVersion': 'v1', 'kind': 'Namespace', 'metadata': {'name': namespace, 'labels': {'spawnpod.dev/rc7-test': identifier, 'pod-security.kubernetes.io/enforce': 'restricted', 'pod-security.kubernetes.io/enforce-version': 'latest'}}})
            created.append((namespace, obj['metadata']['uid']))
        resources = list(yaml.safe_load_all((root / 'deploy/release/source-proxy.kubernetes.example.yaml').read_text()))
        deployment = None
        for resource in resources:
            resource['metadata']['namespace'] = infra
            if resource['kind'] == 'ConfigMap':
                route = {'listen': '0.0.0.0:3128', 'maxConnections': 128, 'routes': [{'host': args.upstream_host, 'port': 443, 'address': args.upstream_ip}]}
                resource['data']['config.yaml'] = yaml.safe_dump(route)
            elif resource['kind'] == 'NetworkPolicy':
                spec = resource['spec']
                spec['ingress'][0]['from'][0]['namespaceSelector']['matchLabels']['kubernetes.io/metadata.name'] = workload
                spec['egress'][0]['to'][0]['ipBlock']['cidr'] = f'{upstream}/32'
            elif resource['kind'] == 'Service':
                resource['spec'].pop('clusterIP')
            elif resource['kind'] == 'Deployment':
                resource['spec']['template']['spec']['containers'][0]['image'] = args.runtime_image
                deployment = resource
                continue
            live = create(resource)
            if resource['kind'] == 'Service':
                proxy_ip = live['spec']['clusterIP']
        config = yaml.safe_load((root / 'deploy/config.example.yaml').read_text())
        config['kubernetes'].update(namespace=workload, leaseNamespace=infra)
        config['agentImage'] = args.runtime_image
        config['profiles']['default']['image'] = args.job_image
        config['profiles']['source'] = dict(config['profiles']['default'], network='source')
        config['sourceProxy'] = f'http://{proxy_ip}:3128'
        config['sourceProxyPolicy'] = args.policy_mode
        config_path = evidence / 'operator-config.yaml'
        config_path.write_text(yaml.safe_dump(config))
        subprocess.run([str(binary), 'upgrade', 'render', '--config', str(config_path), '--target', version, '--image', args.runtime_image, '--runner-image', args.runtime_image, '--output-dir', str(evidence / 'bundle')], check=True, stdout=subprocess.DEVNULL)
        for filename in ('10-network.yaml', '20-admission.yaml'):
            for obj in yaml.safe_load_all((evidence / 'bundle' / filename).read_text()):
                if obj and obj['kind'] == 'NetworkPolicy':
                    create(obj)
        create(deployment)
        k('rollout', 'status', '-n', infra, 'deployment/spawnpod-source-proxy', '--timeout=120s', timeout=130)
        source = probe_pod('source', workload, {'spawnpod.dev/managed': 'true', 'spawnpod.dev/network': 'source'})
        isolated = probe_pod('isolated', workload, {'spawnpod.dev/managed': 'true', 'spawnpod.dev/network': 'isolated'})
        unmanaged = probe_pod('unmanaged', workload, {'spawnpod.dev/network': 'source'})
        outsider = probe_pod('outsider', unauthorized, {'spawnpod.dev/managed': 'true', 'spawnpod.dev/network': 'source'})
        control = probe_pod('control', infra, {})
        for pod in (source, isolated, unmanaged, outsider, control):
            k('wait', '-n', pod['metadata']['namespace'], f"pod/{pod['metadata']['name']}", '--for=condition=Ready', '--timeout=120s', timeout=130)
        dns_ip = get('service', 'kube-dns', 'kube-system')['spec']['clusterIP']
        api_ip = get('service', 'kubernetes', 'default')['spec']['clusterIP']
        control_ip = get('pod', 'control', infra)['status']['podIP']
        proxy_pod = get('pods', namespace=infra)['items']
        proxy_pod = next(p for p in proxy_pod if p['metadata'].get('labels', {}).get('app.kubernetes.io/name') == 'spawnpod-source-proxy')
        save('endpoints.json', {'proxyServiceIP': proxy_ip, 'proxyPodIP': proxy_pod['status']['podIP'], 'upstreamHost': args.upstream_host, 'upstreamIP': args.upstream_ip, 'dnsIP': dns_ip, 'apiIP': api_ip, 'canaryPodIP': control_ip, 'images': [args.runtime_image, args.job_image]})
        # Positive controls establish that denials below are meaningful.
        check(control, 'https-direct', args.upstream_ip, 443)
        check(control, 'tcp-allow', api_ip, 443)
        check(control, 'tcp-allow', control_ip, 8080)
        check(control, 'tcp-allow', control_ip, 3128)
        check(control, 'dns-allow', dns_ip, 53)
        try:
            check(source, 'https-proxy', proxy_ip, 3128)
        except RuntimeError:
            save('proxy-service.json', get('service', 'spawnpod-source-proxy', infra))
            save('proxy-endpoints.json', get('endpointslices', namespace=infra))
            (evidence / 'proxy.log').write_text(k('logs', '-n', infra, proxy_pod['metadata']['name']))
            if args.diagnose_dnat:
                # Diagnostic only: preserve the failing result and re-raise even
                # if backend connectivity works. This is not a deployable pin.
                create({'apiVersion': 'networking.k8s.io/v1', 'kind': 'NetworkPolicy', 'metadata': {'name': 'diagnostic-backend-only', 'namespace': workload}, 'spec': {
                    'podSelector': {'matchLabels': {'spawnpod.dev/managed': 'true', 'spawnpod.dev/network': 'source'}}, 'policyTypes': ['Egress'],
                    'egress': [{'to': [{'ipBlock': {'cidr': proxy_pod['status']['podIP'] + '/32'}}], 'ports': [{'protocol': 'TCP', 'port': 3128}]}]}})
                time.sleep(6)
                check(source, 'https-proxy', proxy_ip, 3128)
                check(source, 'https-proxy', proxy_pod['status']['podIP'], 3128)
            raise
        for mode in ('host-deny', 'port-deny', 'sni-deny'):
            check(source, mode, proxy_ip, 3128)
        for pod in (source, isolated):
            for address, port in ((args.upstream_ip, 443), (api_ip, 443), (dns_ip, 53), (control_ip, 8080), ('169.254.169.254', 80)):
                check(pod, 'tcp-deny', address, port)
            check(pod, 'dns-deny', dns_ip, 53)
        for pod in (isolated, unmanaged, outsider, control):
            check(pod, 'tcp-deny', proxy_ip, 3128)
            check(pod, 'tcp-deny', proxy_pod['status']['podIP'], 3128)
        # A diagnostic fixture under the same egress selector proves policy
        # enforcement without adding shell/Node to the scratch runtime image.
        diagnostic = probe_pod('proxy-egress-diagnostic', infra, {'spawnpod.dev/component': 'source-proxy'})
        k('wait', '-n', infra, 'pod/proxy-egress-diagnostic', '--for=condition=Ready', '--timeout=120s', timeout=130)
        check(diagnostic, 'tcp-allow', args.upstream_ip, 443)
        for address, port in ((api_ip, 443), (dns_ip, 53), (control_ip, 8080), ('169.254.169.254', 80)):
            check(diagnostic, 'tcp-deny', address, port)
        check(diagnostic, 'dns-deny', dns_ip, 53)
        # The component label alone must not grant source access. This Pod
        # accepts TCP/3128, so a denial tests egress rather than a closed port.
        diagnostic_ip = get('pod', 'proxy-egress-diagnostic', infra)['status']['podIP']
        check(source, 'tcp-deny', diagnostic_ip, 3128)
        check(source, 'tcp-deny', control_ip, 3128)
        if args.policy_mode == 'pod-selector':
            check(source, 'https-proxy', proxy_pod['status']['podIP'], 3128)
            k('delete', '-n', infra, 'pod', proxy_pod['metadata']['name'], '--wait=true', '--timeout=60s', timeout=70)
            k('rollout', 'status', '-n', infra, 'deployment/spawnpod-source-proxy', '--timeout=120s', timeout=130)
            replacements = get('pods', namespace=infra)['items']
            replacement = next(p for p in replacements if p['metadata'].get('labels', {}).get('app.kubernetes.io/name') == 'spawnpod-source-proxy')
            if replacement['metadata']['uid'] == proxy_pod['metadata']['uid'] or get('service', 'spawnpod-source-proxy', infra)['spec']['clusterIP'] != proxy_ip:
                raise RuntimeError('proxy replacement or stable Service invariant failed')
            save('proxy-replacement.json', {'oldUID': proxy_pod['metadata']['uid'], 'newUID': replacement['metadata']['uid'], 'oldIP': proxy_pod['status']['podIP'], 'newIP': replacement['status']['podIP'], 'serviceIP': proxy_ip})
            # Readiness probes and CNI reconciliation are independent.
            time.sleep(6)
        check(source, 'https-proxy', proxy_ip, 3128)
    finally:
        # Save only these owned namespaces; never export Secrets or kubeconfig.
        cleanup_errors = []
        for namespace, uid in created:
            try:
                current = get('namespace', namespace)
                if current['metadata']['uid'] != uid or current['metadata']['labels'].get('spawnpod.dev/rc7-test') != identifier:
                    raise RuntimeError('namespace ownership changed; refusing cleanup')
                try:
                    save(f'{namespace}-pods.json', get('pods', namespace=namespace))
                    save(f'{namespace}-policies.json', get('networkpolicies', namespace=namespace))
                    save(f'{namespace}-events.json', get('events', namespace=namespace))
                except Exception as error:
                    cleanup_errors.append(f'evidence {namespace}: {error}')
                k('delete', 'namespace', namespace, '--wait=true', '--timeout=60s', timeout=70)
            except Exception as error:
                print(f'cleanup/evidence error for {namespace}: {error}', flush=True)
                cleanup_errors.append(str(error))
        save('cleanup.json', {'namespaces': [name for name, _ in created], 'errors': cleanup_errors})
        if cleanup_errors:
            raise RuntimeError('acceptance cleanup/evidence failed; inspect cleanup.json')
    save('summary.json', {'passed': True, 'scope': 'local CNI and real proxy Deployment; no Runner/job admission claim', 'assertions': len(results), 'cleanupVerified': True})


if __name__ == '__main__':
    main()
