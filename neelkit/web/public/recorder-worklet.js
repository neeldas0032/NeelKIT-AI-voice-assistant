/**
 * Microphone capture processor.
 *
 * This runs on the audio rendering thread, not the main thread. The older
 * ScriptProcessorNode is deprecated precisely because it ran on the main
 * thread: a React re-render or a garbage collection pause would drop audio
 * frames, which the user hears as clipped words and the recogniser sees as a
 * corrupted utterance.
 *
 * The processor resamples to 16 kHz (the only rate the model accepts) and
 * batches into 20 ms chunks so the socket carries a steady stream of small
 * messages rather than one message per 128-sample render quantum.
 */
class RecorderProcessor extends AudioWorkletProcessor {
  constructor(options) {
    super();
    const opts = (options && options.processorOptions) || {};
    this.targetRate = opts.targetRate || 16000;
    // `sampleRate` is a global provided by the AudioWorklet scope.
    this.ratio = sampleRate / this.targetRate;
    // 20 ms at the target rate.
    this.chunkSize = Math.round(this.targetRate * 0.02);
    this.buffer = new Float32Array(this.chunkSize);
    this.filled = 0;
    // Fractional read position into the incoming stream, carried across render
    // quanta so resampling does not restart (and click) every 128 samples.
    this.position = 0;
    this.muted = false;

    this.port.onmessage = (e) => {
      if (e.data && e.data.type === 'mute') this.muted = !!e.data.value;
    };
  }

  process(inputs) {
    const input = inputs[0];
    if (!input || input.length === 0) return true;

    const channel = input[0];
    if (!channel || channel.length === 0) return true;

    if (this.muted) return true;

    // Linear interpolation is sufficient here: the browser has already
    // band-limited the capture stream to its own Nyquist frequency, and the
    // ratio is small (48k -> 16k). Heavier filtering would cost CPU on the
    // audio thread for no measurable accuracy gain.
    while (this.position < channel.length) {
      const idx = Math.floor(this.position);
      const frac = this.position - idx;
      const a = channel[idx];
      const b = idx + 1 < channel.length ? channel[idx + 1] : a;

      this.buffer[this.filled++] = a + (b - a) * frac;

      if (this.filled === this.chunkSize) {
        // Convert to signed 16-bit PCM: it halves the bytes on the wire versus
        // float32 and is exactly what the server expects.
        const pcm = new Int16Array(this.chunkSize);
        for (let i = 0; i < this.chunkSize; i++) {
          let s = this.buffer[i];
          if (s > 1) s = 1;
          else if (s < -1) s = -1;
          pcm[i] = s < 0 ? s * 0x8000 : s * 0x7fff;
        }
        // Transfer rather than copy: the buffer is not touched again here.
        this.port.postMessage(pcm.buffer, [pcm.buffer]);
        this.filled = 0;
      }

      this.position += this.ratio;
    }

    // Carry the fractional remainder into the next quantum.
    this.position -= channel.length;
    return true;
  }
}

registerProcessor('recorder-processor', RecorderProcessor);
