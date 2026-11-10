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

// BBB's client unpublishes a microphone muted this long
// (DEFAULT_UNPUBLISH_AFTER_MUTE_MS in imports/api/audio/client/bridge/livekit.ts;
// settings.yml turns unpublishOnMute on and leaves the delay unset).
const UNPUBLISH_AFTER_MUTE_MS = 5000;

// unmuteBBBMic waits this long for LiveKit's unmute to reach the page.
// On loopback it takes ms.
const REMOTE_UNMUTE_TIMEOUT_MS = 5000;

// BBB's client publishes its microphone with these options (the bridge's
// publish, and settings.yml's public.media.livekit.audio.publishOptions).
const bbbMicPublishOptions = (name) => ({
  audioPreset: { maxBitrate: 48000, networkPriority: 'high', priority: 'high' },
  dtx: false,
  red: true,
  forceStereo: false,
  name,
});

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
      // Playwright's default, set explicitly: a browser in a network
      // namespace runs as root, where Chromium's sandbox refuses to start.
      chromiumSandbox: false,
      args: [
        '--use-fake-ui-for-media-stream',
        '--use-fake-device-for-media-stream',
        '--autoplay-policy=no-user-gesture-required',
      ],
    });

    return { version: browser.version() };
  },

  // roomOptions, when set, are the Room's options; else livekit-client's
  // defaults apply.
  async join({ peer, url, token, roomOptions }) {
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
    await p.evaluate(async ({ url, token, roomOptions }) => {
      const { Room, RoomEvent } = window.LivekitClient;
      const room = new Room(roomOptions);
      // Subscribed before connect, so the record starts with the join. The
      // clock is the host's, which a network namespace does not change.
      window.e2eRoomEvents = [];
      const record = (event, fields = {}) => window.e2eRoomEvents.push({ event, atUnixMs: Date.now(), ...fields });
      for (const event of [RoomEvent.SignalReconnecting, RoomEvent.Reconnecting, RoomEvent.Reconnected]) {
        room.on(event, () => record(event));
      }
      room.on(RoomEvent.Disconnected, (reason) => record(RoomEvent.Disconnected, { reason: String(reason) }));
      for (const event of [RoomEvent.LocalTrackPublished, RoomEvent.LocalTrackUnpublished]) {
        room.on(event, (pub) => record(event, { sid: pub.trackSid, source: pub.source }));
      }
      room.on(RoomEvent.ConnectionQualityChanged, (quality, participant) => {
        if (participant === room.localParticipant) record(RoomEvent.ConnectionQualityChanged, { quality });
      });
      await room.connect(url, token);
      window.e2eRoom = room;
      window.e2eSender = (trackSid) => {
        const sender = room.localParticipant.trackPublications.get(trackSid)?.track?.sender;
        if (!sender) throw new Error(`no sender for ${trackSid}`);
        return sender;
      };
    }, { url, token, roomOptions });
    peers.set(peer, { context, page: p });

    return {};
  },

  // See PublishBBBMic in browser_test.go. The page keeps the capture's
  // stream, for unmuteBBBMic to publish again.
  async publishBBBMic({ peer, name }) {
    return page(peer).evaluate(async ({ options, unpublishAfterMuteMs }) => {
      const { ParticipantEvent, Track } = window.LivekitClient;
      const room = window.e2eRoom;
      const lp = room.localParticipant;
      const stream = await navigator.mediaDevices.getUserMedia({ audio: {} });
      const mic = { stream, options: { ...options, source: Track.Source.Microphone }, timer: null };
      window.e2eBBBMic = mic;

      const isMic = (pub) => pub.source === Track.Source.Microphone;
      const micPubs = () => [...lp.audioTrackPublications.values()].filter(isMic);
      const unpublishIfMuted = async () => {
        mic.timer = null;
        const pubs = micPubs();
        if (pubs.length === 0 || !pubs.every((pub) => pub.isMuted)) return;
        const stopOnUnpublish = room.options?.stopLocalTrackOnUnpublish ?? false;
        await Promise.all(pubs.map((pub) => lp.unpublishTrack(pub.track, stopOnUnpublish)));
      };

      lp.on(ParticipantEvent.TrackMuted, (pub) => {
        if (!isMic(pub)) return;
        clearTimeout(mic.timer);
        mic.timer = setTimeout(() => {
          unpublishIfMuted().catch((e) => console.error(`unpublish after mute: ${e}`));
        }, unpublishAfterMuteMs);
      });
      lp.on(ParticipantEvent.TrackUnmuted, (pub) => {
        if (!isMic(pub)) return;
        clearTimeout(mic.timer);
        mic.timer = null;
      });

      const pub = await lp.publishTrack(stream.getAudioTracks()[0], mic.options);

      return { trackSid: pub.trackSid };
    }, { options: bbbMicPublishOptions(name), unpublishAfterMuteMs: UNPUBLISH_AFTER_MUTE_MS });
  },

  // See MuteMic in browser_test.go.
  async muteBBBMic({ peer }) {
    await page(peer).evaluate(async () => {
      const { Track } = window.LivekitClient;
      const lp = window.e2eRoom.localParticipant;
      const pub = lp.getTrackPublication(Track.Source.Microphone);
      if (pub && !pub.isMuted) await lp.setMicrophoneEnabled(false);
    });

    return {};
  },

  // See UnmuteMic in browser_test.go. Waits until LiveKit's unmute of a
  // track in remoteUnmuted reaches the page, then does what
  // setSenderTrackEnabled(true) does in BBB's LiveKit audio bridge.
  async unmuteBBBMic({ peer, remoteUnmuted }) {
    return page(peer).evaluate(async ({ remoteUnmuted, timeoutMs }) => {
      const { Track } = window.LivekitClient;
      const mic = window.e2eBBBMic;
      if (!mic) throw new Error('no microphone from publishBBBMic');

      const room = window.e2eRoom;
      const lp = room.localParticipant;
      const micPub = () => lp.getTrackPublication(Track.Source.Microphone);
      const deadline = Date.now() + timeoutMs;
      for (let pub = micPub(); pub?.isMuted && remoteUnmuted.includes(pub.trackSid); pub = micPub()) {
        if (Date.now() > deadline) throw new Error(`LiveKit's unmute of ${pub.trackSid} did not reach the page`);
        await new Promise((resolve) => setTimeout(resolve, 10));
      }

      const pub = micPub();
      if (pub && !pub.isMuted) return { trackSid: pub.trackSid, republished: false };
      if (pub) throw new Error(`microphone ${pub.trackSid} is muted, and LiveKit did not unmute it`);

      if (!mic.stream.active) {
        // BBB captures again through livekit-client, and keeps that capture.
        await lp.setMicrophoneEnabled(true, {}, mic.options);
        const newPub = lp.getTrackPublication(Track.Source.Microphone);
        mic.stream = new MediaStream([newPub.track.mediaStreamTrack]);

        return { trackSid: newPub.trackSid, republished: true };
      }
      const newPub = await lp.publishTrack(mic.stream.getAudioTracks()[0], mic.options);

      return { trackSid: newPub.trackSid, republished: true };
    }, { remoteUnmuted, timeoutMs: REMOTE_UNMUTE_TIMEOUT_MS });
  },

  // Publishes the fake camera as BBB's client does with the camera profile
  // profileId, one of profiles. The capture and the publish options follow
  // bigbluebutton-html5's livekit-camera-bridge (service.ts, component.tsx)
  // and services/livekit/presets.ts. The visible profiles up to the selected
  // one, three at most, become simulcast layers; a hidden profile is
  // published alone.
  //
  // previewMs is CameraCapture.Preview.
  async publishBBBCamera({ peer, name, profiles, profileId, captureFrameRate, previewMs }) {
    return page(peer).evaluate(async ({ name, profiles, profileId, captureFrameRate, previewMs }) => {
      const { Track, VideoPreset } = window.LivekitClient;
      const profile = profiles.find((p) => p.id === profileId);
      if (!profile) throw new Error(`no camera profile ${profileId}`);
      const video = { ...profile.constraints };
      // The capture only: the preset's fps still comes from the profile.
      if (captureFrameRate) video.frameRate = captureFrameRate;
      const stream = await navigator.mediaDevices.getUserMedia({ audio: false, video });
      if (previewMs) await new Promise((resolve) => { setTimeout(resolve, previewMs); });
      const [track] = stream.getVideoTracks();
      const capture = track.getSettings();

      const value = (c) => (typeof c === 'number' ? c : c?.exact ?? c?.ideal ?? c?.max);
      const toPreset = (p) => {
        const width = value(p.constraints?.width);
        const height = value(p.constraints?.height);
        const fps = value(p.constraints?.frameRate);
        if (width && height) return new VideoPreset(width, height, p.bitrate * 1000, fps, 'medium');

        const scale = Math.sqrt(p.bitrate / profile.bitrate);
        const w = Math.max(2, Math.round(capture.width * scale));
        const h = Math.max(2, Math.round(capture.height * scale));

        return new VideoPreset(w - (w % 2), h - (h % 2), p.bitrate * 1000, fps, 'medium');
      };

      const visible = profiles.filter((p) => !p.hidden);
      const selected = visible.findIndex((p) => p.id === profileId);
      const candidates = selected < 0 ? [profile] : visible.slice(Math.max(0, selected - 2), selected + 1);
      const effectiveFps = (p) => Math.min(p.encoding.maxFramerate ?? capture.frameRate, capture.frameRate);
      const presets = [];
      for (const preset of candidates.map(toPreset)) {
        if (!presets.some((p) => p.width === preset.width && p.height === preset.height
          && p.encoding.maxBitrate === preset.encoding.maxBitrate && effectiveFps(p) === effectiveFps(preset))) {
          presets.push(preset);
        }
      }

      const { maxFramerate, ...videoEncoding } = presets[presets.length - 1].encoding;
      if (maxFramerate != null) videoEncoding.maxFramerate = maxFramerate;
      const layers = presets.slice(0, -1);

      const pub = await window.e2eRoom.localParticipant.publishTrack(track, {
        dtx: true,
        videoCodec: 'vp8',
        degradationPreference: 'balanced',
        simulcast: presets.length > 1,
        videoEncoding,
        videoSimulcastLayers: layers.length > 0 ? layers : undefined,
        source: Track.Source.Camera,
        name,
      });
      (window.e2eCameraStreams ??= {})[pub.trackSid] = stream;

      const encodings = pub.track.sender.getParameters().encodings
        .map(({ rid, maxBitrate, maxFramerate }) => ({ rid, maxBitrate, maxFramerate }));

      return {
        trackSid: pub.trackSid,
        capture: { width: capture.width, height: capture.height, frameRate: capture.frameRate },
        encodings,
      };
    }, { name, profiles, profileId, captureFrameRate, previewMs });
  },

  // See UnpublishCamera in browser_test.go.
  async unpublishBBBCamera({ peer, trackSid }) {
    await page(peer).evaluate(async ({ trackSid }) => {
      const lp = window.e2eRoom.localParticipant;
      const pub = lp.trackPublications.get(trackSid);
      if (!pub?.track) throw new Error(`no camera ${trackSid}`);
      window.e2eCameraStreams?.[trackSid]?.getTracks().forEach((track) => track.stop());
      await lp.unpublishTrack(pub.track);
      delete window.e2eCameraStreams?.[trackSid];
    }, { trackSid });

    return {};
  },

  async setCameraLayer({ peer, trackSid, rid, active }) {
    await page(peer).evaluate(async ({ trackSid, rid, active }) => {
      const sender = window.e2eSender(trackSid);
      const params = sender.getParameters();
      const encoding = params.encodings.find((e) => e.rid === rid);
      if (!encoding) throw new Error(`${trackSid} has no layer ${rid}`);
      encoding.active = active;
      await sender.setParameters(params);
    }, { trackSid, rid, active });

    return {};
  },

  async cameraLayers({ peer, trackSid }) {
    return page(peer).evaluate(({ trackSid }) => Object.fromEntries(
      window.e2eSender(trackSid).getParameters().encodings.map((e) => [e.rid, e.active]),
    ), { trackSid });
  },

  // See RoomEvents in browser_test.go.
  async roomEvents({ peer }) {
    return page(peer).evaluate(() => ({ events: window.e2eRoomEvents ?? [] }));
  },

  // See LocalTracks in browser_test.go.
  async localTracks({ peer }) {
    return page(peer).evaluate(() => Object.fromEntries(
      [...window.e2eRoom.localParticipant.trackPublications.values()].map((pub) => [pub.source, pub.trackSid]),
    ));
  },

  // See SenderStats in browser_test.go. A track of one layer has no rid.
  async senderStats({ peer, trackSid }) {
    return page(peer).evaluate(async ({ trackSid }) => {
      const stats = {};
      (await window.e2eSender(trackSid).getStats()).forEach((s) => {
        if (s.type !== 'outbound-rtp') return;
        stats[s.rid ?? ''] = {
          packetsSent: s.packetsSent ?? 0,
          nackCount: s.nackCount ?? 0,
          retransmittedPacketsSent: s.retransmittedPacketsSent ?? 0,
          pliCount: s.pliCount ?? 0,
        };
      });

      return stats;
    }, { trackSid });
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
