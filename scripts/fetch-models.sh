#!/usr/bin/env bash
# Download the speech models NeelKIT needs.
#
# They are not vendored: together they are about 700 MB, they are published by
# other people under their own licences, and they change independently of this
# code. See ATTRIBUTION.md for what each one is and who made it.
set -euo pipefail

DEST="${1:-./models}"
BASE="https://huggingface.co/istupakov/parakeet-tdt-0.6b-v3-onnx/resolve/main"
PRECISION="${NEELKIT_PRECISION:-int8}"

# Pinned by tag AND checksum. A voice-activity model is fetched over the network
# and then trusted with every microphone frame, so a moved tag must fail loudly
# rather than swap the model silently.
VAD_TAG="v6.2.1"
VAD_SHA="1a153a22f4509e292a94e67d6f9b85e8deb25b4988682b7e174c65279d8788e3"

mkdir -p "$DEST"
cd "$DEST"

get() {
  local url="$1" out="$2"
  if [[ -f "$out" ]]; then
    echo "  have $out"
    return
  fi
  echo "  fetching $out"
  curl -fL --progress-bar -o "$out.part" "$url"
  mv "$out.part" "$out"
}

echo "downloading models into $DEST (precision: $PRECISION)"
get "$BASE/config.json" config.json
get "$BASE/vocab.txt" vocab.txt

if [[ "$PRECISION" == "fp32" ]]; then
  get "$BASE/encoder-model.onnx" encoder-model.onnx
  get "$BASE/encoder-model.onnx.data" encoder-model.onnx.data
  get "$BASE/decoder_joint-model.onnx" decoder_joint-model.onnx
else
  get "$BASE/encoder-model.int8.onnx" encoder-model.int8.onnx
  get "$BASE/decoder_joint-model.int8.onnx" decoder_joint-model.int8.onnx
fi

get "https://raw.githubusercontent.com/snakers4/silero-vad/${VAD_TAG}/src/silero_vad/data/silero_vad.onnx" silero_vad.onnx

echo "verifying the voice-activity model"
if command -v sha256sum >/dev/null; then
  echo "${VAD_SHA}  silero_vad.onnx" | sha256sum -c -
elif command -v shasum >/dev/null; then
  echo "${VAD_SHA}  silero_vad.onnx" | shasum -a 256 -c -
else
  echo "  warning: no sha256 tool found; checksum not verified" >&2
fi

echo
echo "done. $(du -sh . | cut -f1) in $DEST"
