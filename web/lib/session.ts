// Realtime session client: microphone capture plus the WebSocket protocol.
//
// Kept out of React entirely. Audio capture and socket lifecycle are imperative
// and long-lived; expressing them as effects produces reconnect loops on every
// re-render. The hook in lib/useSession.ts is a thin subscription over this.

import { API_URL, api } from './api';
import type { ClientMessage, ServerEvent } from './types';

export type ConnectionState =
  | 'idle'
  | 'connecting'
  | 'connected'
  | 'listening'
  | 'error'
  | 'closed';

export interface SessionOptions {
  mode?: 'vad' | 'push_to_talk';
  partials?: boolean;
  hangoverMs?: number;
  conversationId?: string;
  onEvent: (e: ServerEvent) => void;
  onState: (s: ConnectionState, detail?: string) => void;
  /** Called with a 0-1 loudness estimate, for the level meter. */
  onLevel?: (level: number) => void;
  /**
   * Called with SPECTRUM_BANDS values in 0-1, for the waveform display.
   *
   * Real frequency data rather than an animation: a decorative waveform that
   * wiggles while the microphone is muted is worse than no waveform at all,
   * because the one question this control has to answer is "is it hearing me".
   */
  onSpectrum?: (bands: Float32Array) => void;
}

/** Bars in the waveform. Chosen to divide 256 analyser bins evenly. */
export const SPECTRUM_BANDS = 32;

export class RealtimeSession {
  private ws: WebSocket | null = null;
  private ctx: AudioContext | null = null;
  private stream: MediaStream | null = null;
  private node: AudioWorkletNode | null = null;
  private analyser: AnalyserNode | null = null;
  private levelTimer: number | null = null;
  private closedByUs = false;

  constructor(private opts: SessionOptions) {}

  get connected(): boolean {
    return this.ws?.readyState === WebSocket.OPEN;
  }

  /** Opens the socket. Does not touch the microphone. */
  async connect(): Promise<void> {
    this.opts.onState('connecting');
    this.closedByUs = false;

    // A browser cannot set an Authorization header on a WebSocket handshake, so
    // the key is exchanged for a single-use ticket over an authenticated POST.
    // Putting the key in the query string would write it into every access log.
    const { ticket } = await api.ticket();

    const url = new URL(API_URL);
    url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:';
    url.pathname = '/v1/realtime';
    url.searchParams.set('ticket', ticket);

    await new Promise<void>((resolve, reject) => {
      const ws = new WebSocket(url.toString());
      ws.binaryType = 'arraybuffer';
      this.ws = ws;

      const timer = window.setTimeout(() => {
        ws.close();
        reject(new Error('the server did not accept the connection in time'));
      }, 10_000);

      ws.onopen = () => {
        window.clearTimeout(timer);
        this.send({
          type: 'session.configure',
          mode: this.opts.mode ?? 'vad',
          partials: this.opts.partials ?? true,
          hangover_ms: this.opts.hangoverMs,
          conversation_id: this.opts.conversationId,
        });
        this.opts.onState('connected');
        resolve();
      };

      ws.onmessage = (ev) => {
        if (typeof ev.data !== 'string') return;
        try {
          this.opts.onEvent(JSON.parse(ev.data) as ServerEvent);
        } catch {
          /* a malformed frame is not worth tearing the session down for */
        }
      };

      ws.onerror = () => {
        window.clearTimeout(timer);
        reject(new Error('the connection failed'));
      };

      ws.onclose = (ev) => {
        window.clearTimeout(timer);
        this.stopCapture();
        this.opts.onState(
          this.closedByUs ? 'closed' : 'error',
          this.closedByUs ? undefined : ev.reason || 'the connection dropped',
        );
      };
    });
  }

  /** Requests the microphone and starts streaming. */
  async startlistening(): Promise<void> {
    if (!this.connected) throw new Error('not connected');
    if (this.ctx) return;

    if (!navigator.mediaDevices?.getUserMedia) {
      throw new Error(
        'This browser will not share a microphone over an insecure origin. Use localhost or HTTPS.',
      );
    }

    this.stream = await navigator.mediaDevices.getUserMedia({
      audio: {
        channelCount: 1,
        echoCancellation: true,
        noiseSuppression: true,
        autoGainControl: true,
      },
    });

    // Asking for 16 kHz lets the browser resample in native code. The worklet
    // still handles any rate, because Safari ignores this hint.
    this.ctx = new AudioContext({ sampleRate: 16000 });
    await this.ctx.audioWorklet.addModule('/recorder-worklet.js');

    const source = this.ctx.createMediaStreamSource(this.stream);
    this.node = new AudioWorkletNode(this.ctx, 'recorder-processor', {
      processorOptions: { targetRate: 16000 },
    });

    this.node.port.onmessage = (e) => {
      const buf = e.data as ArrayBuffer;
      if (this.ws?.readyState === WebSocket.OPEN) this.ws.send(buf);
    };

    // A separate analyser drives the level meter. Deriving it from the captured
    // chunks instead would couple the visual to the network path, so the meter
    // would freeze whenever the socket stalled — hiding the very problem the
    // user needs to see.
    this.analyser = this.ctx.createAnalyser();
    this.analyser.fftSize = 512;
    source.connect(this.analyser);
    source.connect(this.node);

    // The worklet has no output, so it is not connected to the destination;
    // that also guarantees no feedback loop through the speakers.
    if (this.opts.onLevel || this.opts.onSpectrum) this.startLevelMeter();

    this.opts.onState('listening');
  }

  private startLevelMeter(): void {
    const analyser = this.analyser!;
    const time = new Uint8Array(analyser.fftSize);
    const freq = new Uint8Array(analyser.frequencyBinCount);
    const bands = new Float32Array(SPECTRUM_BANDS);
    const perBand = Math.floor(analyser.frequencyBinCount / SPECTRUM_BANDS);

    const tick = () => {
      analyser.getByteTimeDomainData(time);
      let sum = 0;
      for (let i = 0; i < time.length; i++) {
        const v = (time[i] - 128) / 128;
        sum += v * v;
      }
      const rms = Math.sqrt(sum / time.length);
      // Compress the range: speech rms sits around 0.05-0.2, which would be an
      // almost invisible bar on a linear scale.
      this.opts.onLevel?.(Math.min(1, rms * 4));

      if (this.opts.onSpectrum) {
        analyser.getByteFrequencyData(freq);
        for (let b = 0; b < SPECTRUM_BANDS; b++) {
          let peak = 0;
          for (let i = b * perBand; i < (b + 1) * perBand; i++) {
            if (freq[i] > peak) peak = freq[i];
          }
          // Speech energy is concentrated in the low bins, so a linear map
          // leaves the right two thirds of the display permanently flat. The
          // exponent lifts the quiet high bands without inventing signal.
          bands[b] = Math.pow(peak / 255, 0.7);
        }
        this.opts.onSpectrum(bands);
      }

      // 16 Hz. Fast enough to read as live, slow enough that the whole display
      // is one main-thread paint well inside a frame budget.
      this.levelTimer = window.setTimeout(tick, 60);
    };
    tick();
  }

  /**
   * Stops the microphone but leaves the socket open, and tells the server the
   * utterance is over.
   *
   * Without the commit, push-to-talk never produced a transcript at all — the
   * server was never told the button was released — and in VAD mode anything
   * said in the last moment before the click was thrown away. A commit with
   * nothing in progress is a no-op on the server, so it is always safe to send.
   * It follows the last audio frame on the same socket, so it cannot overtake
   * the speech it is committing.
   */
  stopListening(): void {
    this.stopCapture();
    this.commit();
    if (this.connected) this.opts.onState('connected');
  }

  private stopCapture(): void {
    if (this.levelTimer !== null) {
      window.clearTimeout(this.levelTimer);
      this.levelTimer = null;
    }
    this.opts.onLevel?.(0);
    this.opts.onSpectrum?.(new Float32Array(SPECTRUM_BANDS));

    this.node?.port.close();
    this.node?.disconnect();
    this.node = null;
    this.analyser?.disconnect();
    this.analyser = null;

    // Every track must be stopped explicitly, or the browser keeps showing the
    // recording indicator after the user thinks they stopped.
    this.stream?.getTracks().forEach((t) => t.stop());
    this.stream = null;

    void this.ctx?.close().catch(() => undefined);
    this.ctx = null;
  }

  send(msg: ClientMessage): void {
    if (this.ws?.readyState === WebSocket.OPEN) {
      this.ws.send(JSON.stringify(msg));
    }
  }

  commit(): void {
    this.send({ type: 'session.commit' });
  }

  cancel(): void {
    this.send({ type: 'session.cancel' });
  }

  approve(callId: string): void {
    this.send({ type: 'tool.approve', call_id: callId });
  }

  deny(callId: string, reason = ''): void {
    this.send({ type: 'tool.deny', call_id: callId, reason });
  }

  sendText(text: string): void {
    this.send({ type: 'text.input', text });
  }

  close(): void {
    this.closedByUs = true;
    this.stopCapture();
    this.ws?.close(1000, 'client closed');
    this.ws = null;
    this.opts.onState('closed');
  }
}
