// Native Node probes: no curl, OpenSSL CLI or external dependencies required.
// Invoked inside disposable Restricted Pods by source-network.py.
const net = require('net');
const tls = require('tls');
const https = require('https');
const dgram = require('dgram');
const [mode, address, portText, host] = process.argv.slice(2);
const port = Number(portText);
const timer = setTimeout(() => finish(false, 'probe deadline'), 12000);
function finish(ok, detail) {
  clearTimeout(timer);
  console.log(JSON.stringify({mode, ok, detail}));
  process.exit(ok ? 0 : 1);
}
if (mode === 'tcp-allow' || mode === 'tcp-deny') {
  const denied = mode === 'tcp-deny';
  const socket = net.connect({host: address, port});
  socket.setTimeout(2000, () => finish(denied, 'TCP timeout'));
  socket.on('connect', () => finish(!denied, 'TCP connected'));
  socket.on('error', error => finish(denied, error.code));
} else if (mode === 'dns-allow' || mode === 'dns-deny') {
  // A real UDP DNS query for the configured host, without libc DNS fallback.
  const socket = dgram.createSocket('udp4');
  const name = Buffer.concat(host.split('.').map(label => Buffer.concat([Buffer.from([label.length]), Buffer.from(label)])));
  const packet = Buffer.concat([Buffer.from('123401000001000000000000', 'hex'), name, Buffer.from([0, 0, 1, 0, 1])]);
  socket.on('message', message => finish(mode === 'dns-allow' && message.length >= 12 && message.readUInt16BE(0) === 0x1234 && (message[2] & 0x80) !== 0, 'DNS response'));
  socket.on('error', error => finish(false, error.code));
  socket.send(packet, port, address);
  setTimeout(() => finish(mode === 'dns-deny', 'DNS timeout'), 2000);
} else if (mode === 'https-direct') {
  const request = https.get({host: address, port, servername: host, headers: {Host: host}, path: '/api/v1/version', timeout: 5000}, response => {
    response.resume();
    response.on('end', () => finish(response.statusCode === 200, `HTTPS ${response.statusCode}`));
  });
  request.on('timeout', () => request.destroy(new Error('HTTPS timeout')));
  request.on('error', error => finish(false, error.code || error.message));
} else if (['https-proxy', 'host-deny', 'port-deny', 'sni-deny'].includes(mode)) {
  const socket = net.connect({host: address, port});
  socket.setTimeout(5000, () => finish(false, 'proxy timeout'));
  socket.on('error', error => finish(false, error.code));
  const authority = mode === 'host-deny' ? 'unlisted.example.invalid:443' : `${host}:${mode === 'port-deny' ? 444 : 443}`;
  socket.on('connect', () => socket.write(`CONNECT ${authority} HTTP/1.1\r\nHost: ${authority}\r\n\r\n`));
  let header = Buffer.alloc(0);
  function proxyResponse(data) {
    header = Buffer.concat([header, data]);
    if (header.length > 32768) return finish(false, 'oversized proxy response');
    const end = header.indexOf('\r\n\r\n');
    if (end < 0) return;
    socket.removeListener('data', proxyResponse);
    const status = Number(header.toString('ascii', 0, end).split(' ')[1]);
    if (mode === 'host-deny' || mode === 'port-deny') return finish(status === 403, `CONNECT ${status}`);
    if (status !== 200) return finish(false, `CONNECT ${status}`);
    if (header.length !== end + 4) return finish(false, 'unexpected tunnel bytes');
    socket.removeAllListeners('error');
    const secured = tls.connect({socket, servername: mode === 'sni-deny' ? 'mismatch.example.invalid' : host, rejectUnauthorized: true});
    let established = false;
    secured.on('secureConnect', () => {
      established = true;
      if (mode === 'sni-deny') return finish(false, 'mismatched TLS established');
      secured.write(`GET /api/v1/version HTTP/1.1\r\nHost: ${host}\r\nConnection: close\r\n\r\n`);
    });
    let response = '';
    secured.on('data', bytes => {
      response += bytes.toString('utf8');
      if (response.length > 1048576) finish(false, 'oversized HTTPS response');
    });
    secured.on('error', error => finish(mode === 'sni-deny' && !established, error.code || error.message));
    secured.on('end', () => finish(mode === 'sni-deny' ? !established : /^HTTP\/1\.[01] 200 /.test(response) && response.includes('version'), established ? 'verified HTTPS 200' : 'TLS tunnel closed'));
  }
  socket.on('data', proxyResponse);
} else {
  finish(false, 'unknown probe mode');
}
