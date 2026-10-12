// Test-only HTTPS frontend for the real local Forgejo Git and OCI registry.
// Runs beside the proxy, listens only on Pod loopback, and never logs requests.
const fs = require('fs');
const http = require('http');
const https = require('https');
const net = require('net');
const origin = new URL(process.argv[2]);
if (origin.protocol !== 'http:' || !net.isIP(origin.hostname) || origin.port !== '3001' || origin.username || origin.password || origin.search || origin.hash || !['', '/'].includes(origin.pathname)) throw new Error('invalid local Forgejo origin');
const frontend = 'https://forgejo.rc7.test:3443';
function rewrite(value) {
  return value.replace(/http:\/\/(?:127\.0\.0\.1:3000|localhost:3000)/g, frontend)
    .replace(origin.origin, frontend);
}
const server = https.createServer({key: fs.readFileSync('/tls/tls.key'), cert: fs.readFileSync('/tls/tls.crt')}, (request, response) => {
  if (!request.url.startsWith('/') || request.url.startsWith('//')) { response.writeHead(400).end(); return; }
  const headers = {...request.headers, host: 'forgejo.rc7.test:3443', 'x-forwarded-proto': 'https', 'x-forwarded-host': 'forgejo.rc7.test:3443'};
  delete headers.connection;
  const upstream = http.request({hostname: origin.hostname, port: origin.port, method: request.method, path: request.url, headers, timeout: 30000}, incoming => {
    const outgoing = {...incoming.headers};
    for (const name of ['location', 'www-authenticate']) if (typeof outgoing[name] === 'string') outgoing[name] = rewrite(outgoing[name]);
    response.writeHead(incoming.statusCode, outgoing);
    incoming.pipe(response);
    incoming.on('error', () => response.destroy());
  });
  upstream.on('timeout', () => upstream.destroy());
  upstream.on('error', () => { if (!response.headersSent) response.writeHead(502); response.end(); });
  request.on('aborted', () => upstream.destroy());
  request.pipe(upstream);
});
server.headersTimeout = 5000;
server.requestTimeout = 60000;
server.keepAliveTimeout = 5000;
server.listen(3443, '127.0.0.1', () => console.log('Local TLS fixture ready'));
process.on('SIGTERM', () => { server.closeAllConnections(); server.close(() => process.exit(0)); setTimeout(() => process.exit(1), 3000).unref(); });
