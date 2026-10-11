#!/usr/bin/env python3
"""Forward local test Forgejo to K3s Pods; reject sources outside that test node."""
import argparse
import ipaddress
import selectors
import socket
import socketserver

parser = argparse.ArgumentParser()
parser.add_argument("--bind", required=True)
parser.add_argument("--pod-cidr", required=True)
parser.add_argument("--port", type=int, default=3001)
args = parser.parse_args()
node = ipaddress.ip_address(args.bind)
pods = ipaddress.ip_network(args.pod_cidr)


class Proxy(socketserver.BaseRequestHandler):
    def handle(self):
        source = ipaddress.ip_address(self.client_address[0])
        if source not in pods and source != node:
            return
        with socket.create_connection(("127.0.0.1", 3000), timeout=10) as upstream:
            with selectors.DefaultSelector() as selector:
                selector.register(self.request, selectors.EVENT_READ, upstream)
                selector.register(upstream, selectors.EVENT_READ, self.request)
                while True:
                    events = selector.select(timeout=60)
                    if not events:
                        return
                    for key, _ in events:
                        data = key.fileobj.recv(65536)
                        if not data:
                            return
                        key.data.sendall(data)


class Server(socketserver.ThreadingTCPServer):
    allow_reuse_address = True
    daemon_threads = True


with Server((str(node), args.port), Proxy) as server:
    print("Local Forgejo test proxy ready", flush=True)
    server.serve_forever()
