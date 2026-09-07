const assert = require('node:assert/strict');
const { readFileSync } = require('node:fs');
const { join } = require('node:path');
const { test } = require('node:test');
const vm = require('node:vm');

const html = readFileSync(join(__dirname, '..', 'voice.html'), 'utf8');
const script = html.match(/<script>([\s\S]*?)<\/script>/)[1];
const settle = () => new Promise(resolve => setImmediate(resolve));

function voice() {
  const nodes = new Map();
  const permissions = [], requests = [], peers = [];
  function element(id) {
    if (!nodes.has(id)) nodes.set(id, { value: '', textContent: '', disabled: false, handlers: {}, addEventListener(event, fn) { this.handlers[event] = fn; }, play: async () => {} });
    return nodes.get(id);
  }
  class Peer {
    constructor() { peers.push(this); this.closed = false; }
    close() { this.closed = true; }
    addTrack() {}
    createDataChannel() { return {}; }
    async createOffer() { return { type: 'offer', sdp: 'synthetic offer' }; }
    async setLocalDescription(value) { this.localDescription = value; }
    async setRemoteDescription(value) { this.remoteDescription = value; }
  }
  const context = vm.createContext({
    AbortController, FormData, RTCPeerConnection: Peer,
    document: { getElementById: element },
    window: { isSecureContext: true, addEventListener() {} },
    navigator: { mediaDevices: { getUserMedia: () => new Promise(resolve => permissions.push(resolve)) } },
    fetch: (url, options) => new Promise(resolve => requests.push({ url, options, resolve })),
  });
  vm.runInContext(script, context);
  return { element, permissions, requests, peers, start: () => element('start').handlers.click(), stop: () => element('stop').handlers.click() };
}

function stream() {
  const track = { stopped: false, stop() { this.stopped = true; } };
  return { track, getTracks: () => [track] };
}

test('disconnect during microphone permission stops the late track', async () => {
  const ui = voice(), media = stream();
  const pending = ui.start();
  ui.stop();
  ui.permissions.shift()(media);
  await pending;
  assert.equal(media.track.stopped, true);
  assert.equal(ui.peers[0].closed, true);
  assert.equal(ui.requests.length, 0);
  assert.equal(ui.element('start').disabled, false);
});

test('HTTP rejection releases microphone and presents a retryable error', async () => {
  const ui = voice(), media = stream();
  ui.element('key').value = 'synthetic-key';
  const pending = ui.start();
  ui.permissions.shift()(media);
  await settle();
  assert.equal(ui.requests[0].options.headers.Authorization, 'Bearer synthetic-key');
  ui.requests[0].resolve({ ok: false, status: 403, json: async () => ({ error: { message: 'subscription endpoint rejected the request (forbidden)' } }) });
  await pending;
  assert.match(ui.element('status').textContent, /forbidden/);
  assert.equal(media.track.stopped, true);
  assert.equal(ui.element('start').disabled, false);
  assert.equal(ui.element('stop').disabled, true);
});

test('a late call answer cannot attach to a replacement conversation', async () => {
  const ui = voice();
  const first = ui.start();
  ui.permissions.shift()(stream());
  await settle();
  ui.stop();
  const second = ui.start();
  ui.permissions.shift()(stream());
  await settle();
  ui.requests[0].resolve({ ok: true, text: async () => 'obsolete answer' });
  await first;
  assert.equal(ui.requests[0].options.signal.aborted, true);
  assert.equal(ui.peers[0].remoteDescription, undefined);
  assert.equal(ui.peers[1].remoteDescription, undefined);
  ui.requests[1].resolve({ ok: true, text: async () => 'current answer' });
  await second;
  assert.equal(ui.peers[1].remoteDescription.sdp, 'current answer');
  ui.stop();
});
