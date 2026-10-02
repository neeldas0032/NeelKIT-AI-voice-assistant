#!/usr/bin/env python3
"""Generate a synthetic ASR model with Parakeet TDT's exact I/O signature.

The real checkpoint is 670 MB and cannot live in a repository or a CI run, but
almost everything that can go wrong in the engine is independent of the weights:
tensor shapes, the strided gather of an encoder frame, recurrent-state
plumbing, the duration head advancing the timestep, emit-range ownership across
windows, seam dedup, word assembly from sub-word pieces.

This model makes all of that testable. The decoder is a deterministic finite
state machine over the previous token, so a window of any length produces a
known transcript. If the engine's decode loop is correct, it prints
"hello world this is neelkit"; if any index or state copy is wrong, it does not.
"""
import json
import os
import sys

import numpy as np
import onnx
from onnx import TensorProto as TP
from onnx import helper as H
from onnx import numpy_helper as NH

ENC_DIM = 1024
STATE_DIM = 640
LAYERS = 2
DURATIONS = 5
MELS = 128
SUB = 8

VOCAB = [
    "<unk>", "▁hello", "▁wor", "ld", "▁this",
    "▁is", "▁neel", "kit", "▁a", "▁b",
    "▁c", "▁d", "▁e", "▁f", "▁g", "<blk>",
]
BLANK = 15
V = len(VOCAB)

# next_token[prev] -> emitted token. Following it from the blank start state
# yields: hello wor|ld this is neel|kit, then blank forever.
NEXT = {15: 1, 1: 2, 2: 3, 3: 4, 4: 5, 5: 6, 6: 7, 7: BLANK}


def build_encoder(path):
    audio = H.make_tensor_value_info("audio_signal", TP.FLOAT, [1, MELS, "T"])
    length = H.make_tensor_value_info("length", TP.INT64, [1])
    out = H.make_tensor_value_info("outputs", TP.FLOAT, [1, ENC_DIM, "Tsub"])
    enc_len = H.make_tensor_value_info("encoded_lengths", TP.INT64, [1])

    nodes = [
        # ceil_mode matches the engine's (T-1)/SUB + 1, so the output tensor it
        # preallocates is exactly the shape this graph produces.
        H.make_node("AveragePool", ["audio_signal"], ["pooled"],
                    kernel_shape=[SUB], strides=[SUB], ceil_mode=1),
        H.make_node("Tile", ["pooled", "repeats"], ["outputs"]),
        H.make_node("Shape", ["pooled"], ["shape"]),
        H.make_node("Gather", ["shape", "axis2"], ["tsub"], axis=0),
        H.make_node("Unsqueeze", ["tsub", "zero"], ["encoded_lengths"]),
    ]
    inits = [
        NH.from_array(np.array([1, ENC_DIM // MELS, 1], np.int64), "repeats"),
        NH.from_array(np.array(2, np.int64), "axis2"),
        NH.from_array(np.array([0], np.int64), "zero"),
    ]
    g = H.make_graph(nodes, "synthetic_encoder", [audio, length], [out, enc_len], inits)
    m = H.make_model(g, opset_imports=[H.make_opsetid("", 17)])
    m.ir_version = 9
    onnx.checker.check_model(m)
    onnx.save(m, path)


def build_decoder(path):
    table = np.full((V, V + DURATIONS), -8.0, np.float32)
    for prev in range(V):
        nxt = NEXT.get(prev, BLANK)
        table[prev, nxt] = 12.0
        # Duration class 1: advance exactly one encoder frame per step, so the
        # engine's timestep arithmetic is exercised on every iteration.
        table[prev, V + 1] = 6.0

    enc = H.make_tensor_value_info("encoder_outputs", TP.FLOAT, [1, ENC_DIM, 1])
    tgt = H.make_tensor_value_info("targets", TP.INT32, [1, 1])
    tlen = H.make_tensor_value_info("target_length", TP.INT32, [1])
    s1 = H.make_tensor_value_info("input_states_1", TP.FLOAT, [LAYERS, 1, STATE_DIM])
    s2 = H.make_tensor_value_info("input_states_2", TP.FLOAT, [LAYERS, 1, STATE_DIM])

    out = H.make_tensor_value_info("outputs", TP.FLOAT, [1, 1, 1, V + DURATIONS])
    o1 = H.make_tensor_value_info("output_states_1", TP.FLOAT, [LAYERS, 1, STATE_DIM])
    o2 = H.make_tensor_value_info("output_states_2", TP.FLOAT, [LAYERS, 1, STATE_DIM])

    nodes = [
        H.make_node("Cast", ["targets"], ["idx"], to=TP.INT64),
        H.make_node("Gather", ["table", "idx"], ["row"], axis=0),   # [1,1,V+D]
        H.make_node("Unsqueeze", ["row", "zero"], ["outputs"]),     # [1,1,1,V+D]
        # The state is incremented rather than passed through, so a missing
        # state copy in the decode loop changes observable behaviour instead of
        # being silently equivalent.
        H.make_node("Add", ["input_states_1", "one"], ["output_states_1"]),
        H.make_node("Add", ["input_states_2", "one"], ["output_states_2"]),
    ]
    inits = [
        NH.from_array(table, "table"),
        NH.from_array(np.array([0], np.int64), "zero"),
        NH.from_array(np.array(1.0, np.float32), "one"),
    ]
    g = H.make_graph(nodes, "synthetic_decoder_joint",
                     [enc, tgt, tlen, s1, s2], [out, o1, o2], inits)
    m = H.make_model(g, opset_imports=[H.make_opsetid("", 17)])
    m.ir_version = 9
    onnx.checker.check_model(m)
    onnx.save(m, path)


def main():
    out = sys.argv[1] if len(sys.argv) > 1 else "testdata/synthetic-model"
    os.makedirs(out, exist_ok=True)
    build_encoder(os.path.join(out, "encoder-model.onnx"))
    build_decoder(os.path.join(out, "decoder_joint-model.onnx"))
    with open(os.path.join(out, "config.json"), "w") as f:
        json.dump({"model_type": "synthetic-tdt",
                   "features_size": MELS,
                   "subsampling_factor": SUB}, f, indent=2)
    with open(os.path.join(out, "vocab.txt"), "w") as f:
        for i, p in enumerate(VOCAB):
            f.write(f"{p} {i}\n")
    print(f"wrote synthetic model to {out}")
    print("expected transcript: 'hello world this is neelkit'")


if __name__ == "__main__":
    main()
