#!/usr/bin/env python3
"""Serve one local OCI archive through a read-only loopback registry fixture.

For a single-node local K3s preview: avoids node filesystem writes and privileged
importer Pods. Not a production registry or a publication/signature mechanism.
"""
import argparse
import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
from pathlib import Path
import re
import shutil
import tarfile
from urllib.parse import unquote, urlparse


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--archive', required=True, type=Path)
    parser.add_argument('--reference-file', required=True, type=Path)
    parser.add_argument('--port', type=int, default=5007)
    parser.add_argument('--repository', default='spawnpod-preview')
    args = parser.parse_args()
    if not re.fullmatch(r'[a-z0-9][a-z0-9-]*', args.repository):
        parser.error('repository must be one lowercase name')
    archive_path = args.archive.resolve()
    blobs = {}
    with tarfile.open(archive_path) as archive:
        index = json.load(archive.extractfile('index.json'))
        if len(index['manifests']) != 1:
            parser.error('fixture requires a single image manifest')
        descriptor = index['manifests'][0]
        digest = descriptor['digest']
        for member in archive.getmembers():
            if re.fullmatch(r'blobs/sha256/[a-f0-9]{64}', member.name) and member.isfile():
                with archive.extractfile(member) as stream:
                    actual = hashlib.file_digest(stream, 'sha256').hexdigest()
                if member.name.split('/')[-1] != actual:
                    parser.error('archive blob checksum mismatch')
                blobs['sha256:' + actual] = (member.name, member.size)
        manifest = json.load(archive.extractfile(blobs[digest][0]))
        if 'config' not in manifest or 'layers' not in manifest:
            parser.error('fixture requires an image, not a multiarch index')
        media_type = descriptor['mediaType']

    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass

        def do_HEAD(self):
            self.serve(False)

        def do_GET(self):
            self.serve(True)

        def serve(self, body):
            path = unquote(urlparse(self.path).path)
            if path.rstrip('/') == '/v2':
                self.send_response(200)
                self.send_header('Docker-Distribution-API-Version', 'registry/2.0')
                self.send_header('Content-Length', '2')
                self.end_headers()
                if body:
                    self.wfile.write(b'{}')
                return
            prefix = '/v2/' + args.repository + '/'
            if not path.startswith(prefix):
                self.send_error(404)
                return
            tail = path[len(prefix):]
            category, separator, requested = tail.partition('/')
            if not separator or category not in ('manifests', 'blobs') or requested not in blobs or (category == 'manifests' and requested != digest):
                self.send_error(404)
                return
            member, size = blobs[requested]
            self.send_response(200)
            self.send_header('Content-Type', media_type if category == 'manifests' else 'application/octet-stream')
            self.send_header('Docker-Content-Digest', requested)
            self.send_header('Content-Length', str(size))
            self.end_headers()
            if body:
                with tarfile.open(archive_path) as archive, archive.extractfile(member) as stream:
                    shutil.copyfileobj(stream, self.wfile)

    server = ThreadingHTTPServer(('127.0.0.1', args.port), Handler)
    args.reference_file.write_text(f'localhost:{server.server_port}/{args.repository}@{digest}\n')
    print(f'Loopback preview registry ready on 127.0.0.1:{server.server_port}', flush=True)
    try:
        server.serve_forever()
    finally:
        server.server_close()


if __name__ == '__main__':
    main()
