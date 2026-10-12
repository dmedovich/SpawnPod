// Bounded OCI Distribution transport fixture, executed under the job seccomp.
// Uses HTTPS_PROXY explicitly; checks certificates, redirects and content hashes.
const fs = require('fs');
const http = require('http');
const https = require('https');
const tls = require('tls');
const net = require('net');
const crypto = require('crypto');
const assert = require('assert/strict');
const origin = new URL('https://forgejo.rc7.test:3443');
const proxy = new URL(process.env.HTTPS_PROXY);
assert.equal(proxy.protocol, 'http:');
assert(net.isIP(proxy.hostname) && proxy.port && !proxy.username && !proxy.password);
const ca = fs.readFileSync(process.env.RC7_CA_FILE);
const credential = process.env.RC7_OCI_TOKEN;
assert(credential && /^[a-f0-9]{40}$/.test(credential));
const name = process.env.RC7_PACKAGE;
assert(/^rc7-transport-[a-f0-9]{32}$/.test(name));
const owner = process.env.RC7_OCI_OWNER;
assert(/^rc7-oci-[a-f0-9]{16}$/.test(owner));
const repository = `${owner}/${name}`;
const basic = 'Basic ' + Buffer.from(`${owner}:${credential}`).toString('base64');
let bearer;
class ProxyAgent extends https.Agent {
  createConnection(options, callback) {
    let completed = false;
    const done = (error, socket) => { if (!completed) { completed = true; callback(error, socket); } };
    const connect = http.request({hostname: proxy.hostname, port: proxy.port, method: 'CONNECT', path: origin.host, headers: {Host: origin.host}, timeout: 5000});
    connect.on('error', error => done(error));
    connect.on('timeout', () => connect.destroy(new Error('proxy timeout')));
    connect.on('connect', (response, socket, head) => {
      if (response.statusCode !== 200 || head.length) { socket.destroy(); done(new Error('proxy rejected tunnel')); return; }
      const secure = tls.connect({socket, servername: origin.hostname, ca, rejectUnauthorized: true});
      secure.setTimeout(10000, () => secure.destroy(new Error('TLS timeout')));
      secure.once('error', error => done(error));
      secure.once('secureConnect', () => done(null, secure));
    });
    connect.end();
  }
}
const agent = new ProxyAgent({keepAlive: false, maxSockets: 4});
function target(path) {
  const url = new URL(path, origin);
  assert.equal(url.origin, origin.origin, 'registry must not redirect credentials to another origin');
  assert(!url.username && !url.password && !url.hash);
  return url;
}
function exchange(method, path, body, headers) {
  return new Promise((resolve, reject) => {
    const request = https.request(target(path), {method, agent, headers: {...headers, ...(body ? {'Content-Length': body.length} : {})}, timeout: 15000}, response => {
      const chunks = []; let size = 0;
      response.on('data', chunk => { size += chunk.length; if (size > 1048576) response.destroy(new Error('oversized registry response')); else chunks.push(chunk); });
      response.on('error', reject);
      response.on('end', () => resolve({status: response.statusCode, headers: response.headers, body: Buffer.concat(chunks)}));
    });
    request.on('error', reject);
    request.on('timeout', () => request.destroy(new Error('registry timeout')));
    request.end(body);
  });
}
async function request(method, path, body, headers = {}) {
  let result = await exchange(method, path, body, {Authorization: bearer ? 'Bearer ' + bearer : basic, ...headers});
  if (result.status === 401 && !headers.Authorization) {
    const challenge = result.headers['www-authenticate'] || '';
    assert(challenge.startsWith('Bearer '), 'expected registry bearer challenge');
    const attributes = Object.fromEntries([...challenge.matchAll(/([a-z]+)="([^"]*)"/g)].map(match => [match[1], match[2]]));
    const realm = target(attributes.realm);
    for (const key of ['service', 'scope']) if (attributes[key]) realm.searchParams.set(key, attributes[key]);
    const authorization = await exchange('GET', realm.href, undefined, {Authorization: basic});
    assert.equal(authorization.status, 200, 'scoped package credential rejected');
    const tokens = JSON.parse(authorization.body);
    bearer = tokens.token || tokens.access_token;
    assert.equal(typeof bearer, 'string');
    result = await exchange(method, path, body, {Authorization: 'Bearer ' + bearer, ...headers});
  }
  return result;
}
const digest = data => 'sha256:' + crypto.createHash('sha256').update(data).digest('hex');
const descriptor = (data, mediaType) => ({mediaType, digest: digest(data), size: data.length});
async function pushBlob(data) {
  const begin = await request('POST', `/v2/${repository}/blobs/uploads/`);
  assert.equal(begin.status, 202, 'begin blob upload');
  const location = target(begin.headers.location);
  try {
    location.searchParams.set('digest', digest(data));
    const commit = await request('PUT', location.href, data, {'Content-Type': 'application/octet-stream'});
    assert.equal(commit.status, 201, 'commit blob');
    const downloaded = await request('GET', `/v2/${repository}/blobs/${digest(data)}`);
    assert.equal(downloaded.status, 200, 'pull blob');
    assert.equal(digest(downloaded.body), digest(data), 'blob content changed');
  } catch (error) {
    location.searchParams.delete('digest');
    const removed = await request('DELETE', location.href);
    assert([204, 404].includes(removed.status), 'failed upload cleanup');
    throw error;
  }
}
const deadline = setTimeout(() => { console.error('OCI fixture deadline'); process.exit(1); }, 120000);
(async () => {
  const unauthenticated = await exchange('POST', `/v2/${repository}/blobs/uploads/`, undefined, {});
  assert.equal(unauthenticated.status, 401, 'unauthenticated writes must be denied');
  const packagePath = `/api/v1/packages/${owner}/container/${name}/latest`;
  const absent = await request('GET', packagePath, undefined, {Authorization: 'token ' + credential});
  assert.equal(absent.status, 404, 'test package must be absent before upload');
  let committed = false;
  try {
    const config = Buffer.from('{}');
    const payload = Buffer.from('SpawnPod RC7 ready-made OCI artifact\n');
    await pushBlob(config);
    await pushBlob(payload);
    const mediaType = 'application/vnd.oci.image.manifest.v1+json';
    const manifest = Buffer.from(JSON.stringify({schemaVersion: 2, mediaType, artifactType: 'application/vnd.spawnpod.rc7.fixture.v1', config: descriptor(config, 'application/vnd.oci.empty.v1+json'), layers: [descriptor(payload, 'application/octet-stream')]}));
    const pushed = await request('PUT', `/v2/${repository}/manifests/latest`, manifest, {'Content-Type': mediaType});
    assert.equal(pushed.status, 201, 'push OCI manifest');
    committed = true;
    assert.equal(pushed.headers['docker-content-digest'], digest(manifest));
    const pulled = await request('GET', `/v2/${repository}/manifests/${digest(manifest)}`, undefined, {Accept: mediaType});
    assert.equal(pulled.status, 200, 'pull OCI manifest by digest');
    assert.equal(digest(pulled.body), digest(manifest), 'manifest digest changed');
    console.log(JSON.stringify({passed: true, repository, manifestDigest: digest(manifest), transport: 'verified TLS through HTTPS_PROXY', unauthorizedWriteDenied: true}));
  } finally {
    if (committed) {
      const removed = await request('DELETE', packagePath, undefined, {Authorization: 'token ' + credential});
      assert.equal(removed.status, 204, 'remove disposable OCI version');
      const absent = await request('GET', packagePath, undefined, {Authorization: 'token ' + credential});
      assert.equal(absent.status, 404, 'OCI package cleanup');
      console.log('Disposable OCI version removed and absence verified');
    }
  }
})().then(() => { clearTimeout(deadline); agent.destroy(); }).catch(error => { clearTimeout(deadline); agent.destroy(); console.error('OCI transport assertion failed: ' + error.message); process.exitCode = 1; });
