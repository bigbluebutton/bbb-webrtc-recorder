// Drives a browser for the recorder's E2E suite (tests/e2e/browser_test.go).
//
// It reads one request per line on stdin,
//   {"id": n, "method": "...", "params": {...}}
// and writes one reply per line on stdout,
//   {"id": n, "result": {...}} or {"id": n, "error": "..."}.
// Logs, the pages' console included, go to stderr. When stdin closes, it
// closes the browser and exits.

import { readdirSync, readFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import readline from 'node:readline';
import { chromium } from 'playwright';

// The UMD build defines window.LivekitClient in the page.
const livekitClient = createRequire(import.meta.url).resolve('livekit-client');

let browser;
const peers = new Map();
let freeze = null;

const log = (msg) => process.stderr.write(`${new Date().toISOString()} ${msg}\n`);

function page(peer) {
  const p = peers.get(peer);
  if (!p) throw new Error(`no peer ${peer}`);

  return p.page;
}

const methods = {
  // The 'chromium' channel runs Playwright's full Chromium build in new
  // headless mode, the browser users run, not its headless shell.
  async launch({ executablePath }) {
    browser = await chromium.launch({
      channel: executablePath ? undefined : 'chromium',
      executablePath: executablePath || undefined,
      args: [
        '--use-fake-ui-for-media-stream',
        '--use-fake-device-for-media-stream',
        '--autoplay-policy=no-user-gesture-required',
      ],
    });

    return { version: browser.version() };
  },

  async join({ peer, url, token }) {
    // The routed page has no address of its own. Chromium's Local Network
    // Access treats it as outside the loopback address space, so it refuses
    // the page's connections to LiveKit on 127.0.0.1 unless this is granted.
    const context = await browser.newContext({ permissions: ['local-network-access'] });
    const p = await context.newPage();
    p.on('console', (m) => log(`[${peer}] ${m.type()}: ${m.text()}`));
    p.on('pageerror', (e) => log(`[${peer}] page error: ${e.message}`));
    // getUserMedia needs a secure context, which localhost is. Nothing
    // listens there: the route serves the page.
    await p.route('http://localhost/', (r) => r.fulfill({ contentType: 'text/html', body: '<!doctype html><title>e2e</title>' }));
    await p.goto('http://localhost/');
    await p.addScriptTag({ path: livekitClient });
    await p.evaluate(async ({ url, token }) => {
      const room = new window.LivekitClient.Room();
      await room.connect(url, token);
      window.e2eRoom = room;
    }, { url, token });
    peers.set(peer, { context, page: p });

    return {};
  },

  async publishMic({ peer, name, dtx, red }) {
    const trackSid = await page(peer).evaluate(async ({ name, dtx, red }) => {
      const { Track, createLocalAudioTrack } = window.LivekitClient;
      const track = await createLocalAudioTrack();
      const pub = await window.e2eRoom.localParticipant.publishTrack(track, {
        source: Track.Source.Microphone, name, dtx, red,
      });

      return pub.trackSid;
    }, { name, dtx, red });

    return { trackSid };
  },

  // See CaptureFreeze in browser_test.go. A signal that fails, as when the
  // process is gone, ends the freeze, and thawCapture reports it.
  async freezeCapture({ burstMs, everyMs }) {
    if (freeze) throw new Error('the capture is already freezing');
    const pid = audioServicePid();
    const f = { pid, stops: 0, frozenMs: 0, maxBurstMs: 0, since: 0, timer: null, error: null };
    const signal = (sig) => {
      try {
        process.kill(pid, sig);
        return true;
      } catch (e) {
        f.error = `${sig} to the audio service (pid ${pid}): ${e.message}`;
        return false;
      }
    };
    const resume = () => {
      if (!signal('SIGCONT')) return false;
      const burst = performance.now() - f.since;
      f.frozenMs += burst;
      f.maxBurstMs = Math.max(f.maxBurstMs, burst);
      f.since = 0;

      return true;
    };
    const cont = () => {
      if (resume()) f.timer = setTimeout(stop, everyMs - burstMs);
    };
    const stop = () => {
      if (!signal('SIGSTOP')) return;
      f.since = performance.now();
      f.stops++;
      f.timer = setTimeout(cont, burstMs);
    };
    f.thaw = () => {
      clearTimeout(f.timer);
      if (f.since) resume();
    };
    f.timer = setTimeout(stop, everyMs - burstMs);
    freeze = f;

    return { pid };
  },

  async thawCapture() {
    if (!freeze) throw new Error('the capture is not freezing');
    const f = freeze;
    freeze = null;
    f.thaw();
    if (f.error) throw new Error(f.error);

    return { stops: f.stops, frozenMs: f.frozenMs, maxBurstMs: f.maxBurstMs };
  },

  async close() {
    try {
      freeze?.thaw();
    } finally {
      freeze = null;
      await browser?.close();
    }

    return {};
  },
};

// audioServicePid finds the browser's audio service: a utility process
// descended from this one.
function audioServicePid() {
  const children = new Map();
  for (const name of readdirSync('/proc')) {
    if (!/^\d+$/.test(name)) continue;
    let stat;
    try {
      stat = readFileSync(`/proc/${name}/stat`, 'utf8');
    } catch {
      continue; // exited
    }
    // The command name in parentheses can hold spaces; the parent PID is the
    // second field after it.
    const ppid = Number(stat.slice(stat.lastIndexOf(')') + 2).split(' ')[1]);
    if (!children.has(ppid)) children.set(ppid, []);
    children.get(ppid).push(Number(name));
  }

  const found = [];
  const queue = [process.pid];
  while (queue.length) {
    for (const pid of children.get(queue.shift()) ?? []) {
      queue.push(pid);
      let cmdline = '';
      try {
        cmdline = readFileSync(`/proc/${pid}/cmdline`, 'utf8');
      } catch {
        continue;
      }
      if (cmdline.includes('--utility-sub-type=audio.mojom.AudioService')) found.push(pid);
    }
  }
  if (found.length === 0) {
    throw new Error('no audio service process: publish a microphone first, or the browser runs audio in process');
  }
  if (found.length > 1) throw new Error(`${found.length} audio service processes`);

  return found[0];
}

const reply = (msg) => process.stdout.write(`${JSON.stringify(msg)}\n`);

const rl = readline.createInterface({ input: process.stdin });
rl.on('line', async (line) => {
  let req;
  try {
    req = JSON.parse(line);
  } catch {
    log(`not a request: ${line}`);
    return;
  }
  try {
    const fn = methods[req.method];
    if (!fn) throw new Error(`unknown method ${req.method}`);
    reply({ id: req.id, result: (await fn(req.params ?? {})) ?? {} });
  } catch (e) {
    reply({ id: req.id, error: String(e?.stack ?? e) });
  }
});
rl.on('close', async () => {
  try {
    await methods.close();
  } finally {
    process.exit(0);
  }
});
