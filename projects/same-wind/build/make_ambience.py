# make_ambience.py - 《同一阵风》环境声合成
#
# 剧本 style 明确约定：全片无配乐，只有风、柴油机、麦秆摩擦（tts_notes：
# 环境声为实录音效）。本项目没有实录素材库，这里用程序合成替代：
# 每个镜头一段 22s 环境床（镜头 19.9s + 余量），边缘 0.8s 淡入淡出，
# 相邻镜头的床在剪切点附近交叠，避免风声在切点处瞬间断掉。
#
# 命名遵循 deliver 的音效约定：<shot_id>.sfx.wav → 挂到对应镜头起点，
# 混音时按 config.audio.sfx_gain_db(-14dB) 进音效子混音。
#
# 用法: python make_ambience.py <run_dir>
import math
import os
import random
import struct
import sys
import wave

SR = 48000
DUR = 22.0
FADE = 0.8

def lowpass(samples, cutoff, sr=SR, stages=2):
    """一阶低链级联：白噪声 -> 频谱倾斜，模拟空气吸声（远处的声音更闷）。"""
    out = samples
    for _ in range(stages):
        alpha = 1.0 - math.exp(-2.0 * math.pi * cutoff / sr)
        prev = 0.0
        res = []
        for x in out:
            prev += alpha * (x - prev)
            res.append(prev)
        out = res
    return out

def highpass(samples, cutoff, sr=SR):
    alpha = 0.85 * math.exp(-2.0 * math.pi * cutoff / sr) + 0.1
    prev_in = prev_out = 0.0
    res = []
    for x in samples:
        prev_out = alpha * (prev_out + x - prev_in)
        prev_in = x
        res.append(prev_out)
    return res

def normalize(samples, target_rms):
    n = len(samples)
    rms = math.sqrt(sum(x * x for x in samples) / max(1, n))
    if rms < 1e-9:
        return samples
    g = target_rms / rms
    return [x * g for x in samples]

def envelope(n, sr, lfo_hz, lfo_depth, gust_every, rng):
    """风的速度包络：慢 LFO 起伏 + 随机阵风抬升。"""
    env = []
    phase = rng.random() * 6.283
    next_gust = rng.uniform(0.5 * gust_every, gust_every)
    gust_level = 1.0
    for i in range(n):
        t = i / sr
        base = 1.0 - lfo_depth + lfo_depth * (0.5 + 0.5 * math.sin(2 * math.pi * lfo_hz * t + phase))
        if t >= next_gust:
            gust_level = rng.uniform(1.2, 1.9)
            next_gust = t + rng.uniform(0.6 * gust_every, 1.4 * gust_every)
        gust_level += (1.0 - gust_level) * 0.0004  # 阵风缓慢衰减回基线
        env.append(base * gust_level)
    return env

def wind_bed(sr, dur, rng, cutoff=520, lfo_hz=0.07, lfo_depth=0.45, gust_every=4.0, rms=0.05):
    n = int(sr * dur)
    raw = [rng.gauss(0, 1) for _ in range(n)]
    lp = lowpass(raw, cutoff, sr, stages=2)
    env = envelope(n, sr, lfo_hz, lfo_depth, gust_every, rng)
    return normalize([s * e for s, e in zip(lp, env)], rms)

def diesel_bed(sr, dur, rng, fire_hz=19.0, tone_hz=52.0, brightness=0.35, wobble=0.12, rms=0.06):
    """柴油机：点火频率的脉冲感 + 低频体感 + 随转速的轻微抖动。"""
    n = int(sr * dur)
    out = []
    ph = rng.random() * 6.283
    for i in range(n):
        t = i / sr
        w = 1.0 + wobble * math.sin(2 * math.pi * 0.11 * t + ph)
        # 点火脉冲：半波整流的正弦串，谐波丰富但克制
        pulse = max(0.0, math.sin(2 * math.pi * fire_hz * w * t))
        pulse = pulse ** 1.6
        # 低频体感
        body = math.sin(2 * math.pi * tone_hz * w * t)
        out.append(0.6 * pulse + brightness * body)
    out = lowpass(out, 900, sr, stages=1)
    slow = envelope(n, sr, 0.05, 0.18, 7.0, rng)
    return normalize([s * e for s, e in zip(out, slow)], rms)

def rustle_bed(sr, dur, rng, density=9.0, cutoff_hi=4200, rms=0.045):
    """麦秆摩擦：高通噪声上的随机短促爆点（断续的沙沙声）。"""
    n = int(sr * dur)
    raw = [rng.gauss(0, 1) for _ in range(n)]
    band = highpass(lowpass(raw, cutoff_hi, sr, stages=1), 700)
    out = [0.0] * n
    t_next = 0.0
    while t_next < dur:
        center = t_next
        width = rng.uniform(0.06, 0.25)
        amp = rng.uniform(0.4, 1.0)
        i0, i1 = int(center * sr), min(n, int((center + width) * sr))
        for i in range(i0, i1):
            u = (i - i0) / max(1, i1 - i0)
            out[i] += amp * math.sin(math.pi * u) ** 2
        t_next += rng.expovariate(density)
    return normalize([s * e for s, e in zip(band, out)], rms)

def write_stereo(path, left, right, sr=SR, fade=FADE):
    assert len(left) == len(right)
    n = len(left)
    nf = int(fade * sr)
    frames = bytearray()
    for i in range(n):
        g = 1.0
        if i < nf:
            g = i / nf
        elif i > n - nf:
            g = max(0.0, (n - i) / nf)
        for ch in (left, right):
            v = max(-1.0, min(1.0, ch[i] * g))
            frames += struct.pack("<h", int(v * 32767))
    with wave.open(path, "wb") as w:
        w.setnchannels(2)
        w.setsampwidth(2)
        w.setframerate(sr)
        w.writeframes(bytes(frames))

def make_shot(sr, dur, seed, layers):
    """layers: [(generator, level), ...] 左右声道分别取独立噪声种子（去相关立体声）。"""
    out_l, out_r = [], []
    n = int(sr * dur)
    for gen, level in layers:
        l = gen(sr, dur, random.Random(seed))
        r = gen(sr, dur, random.Random(seed + 977))
        out_l = mix(out_l, l, level, n)
        out_r = mix(out_r, r, level, n)
    return out_l, out_r

def mix(base, add, level, n):
    if not base:
        return [x * level for x in add]
    return [b + a * level for b, a in zip(base, add)]

def make_shot_layer(kind, **kw):
    return lambda sr, dur, rng: _dispatch(sr, dur, rng, kind, **kw)

def _dispatch(sr, dur, rng, kind, **kw):
    if kind == "wind":
        return wind_bed(sr, dur, rng, **kw)
    if kind == "diesel":
        return diesel_bed(sr, dur, rng, **kw)
    if kind == "rustle":
        return rustle_bed(sr, dur, rng, **kw)
    raise ValueError(kind)

# 每镜头的声音设计（对白隙里的"背景声音"就是它们）：
SHOTS = {
    # sw001 大远景麦田夜：整片麦浪的风，宽、低、无起伏的推拉
    "sw001": [("wind", make_shot_layer("wind", cutoff=430, lfo_hz=0.05, lfo_depth=0.5, gust_every=6.0, rms=0.055))],
    # sw002 收割机舰队在地平线：远处的机群低鸣（很闷）+ 风
    "sw002": [("wind", make_shot_layer("wind", cutoff=480, lfo_hz=0.06, lfo_depth=0.4, gust_every=5.0, rms=0.04)),
              ("diesel", make_shot_layer("diesel", fire_hz=13.0, tone_hz=44.0, brightness=0.25, rms=0.05))],
    # sw003 探照灯扫过田埂：风更近，发动机嗡鸣比 sw002 抬升一点
    "sw003": [("wind", make_shot_layer("wind", cutoff=650, lfo_hz=0.08, lfo_depth=0.45, gust_every=4.0, rms=0.055)),
              ("diesel", make_shot_layer("diesel", fire_hz=16.0, tone_hz=55.0, brightness=0.3, rms=0.04))],
    # sw004 近景按麦茬：麦秆断口的脆响 + 贴地风
    "sw004": [("wind", make_shot_layer("wind", cutoff=700, lfo_hz=0.09, lfo_depth=0.5, gust_every=3.2, rms=0.05)),
              ("rustle", make_shot_layer("rustle", density=11.0, rms=0.05))],
    # sw005 院子夜：全片最静的一段，风弱下来，远处有一台怠速的柴油机
    "sw005": [("wind", make_shot_layer("wind", cutoff=400, lfo_hz=0.045, lfo_depth=0.35, gust_every=7.0, rms=0.028)),
              ("diesel", make_shot_layer("diesel", fire_hz=9.0, tone_hz=38.0, brightness=0.18, wobble=0.08, rms=0.02))],
    # sw006 夜路离开（车内视点）：柴油机的脉动成为主导，风从车侧掠过
    "sw006": [("diesel", make_shot_layer("diesel", fire_hz=21.0, tone_hz=62.0, brightness=0.4, rms=0.065)),
              ("wind", make_shot_layer("wind", cutoff=850, lfo_hz=0.1, lfo_depth=0.35, gust_every=3.5, rms=0.035)),
              ("rustle", make_shot_layer("rustle", density=3.0, cutoff_hi=2600, rms=0.012))],
}

# 每镜头整体响度系数（合成后再归一到它，保住强弱层次）
SHOT_LEVEL = {"sw001": 1.0, "sw002": 0.95, "sw003": 1.0, "sw004": 1.0, "sw005": 0.6, "sw006": 1.0}

def main():
    run_dir = sys.argv[1] if len(sys.argv) > 1 else "."
    out_dir = os.path.join(run_dir, "output", "sfx")
    os.makedirs(out_dir, exist_ok=True)
    seed0 = 20260926
    for idx, (shot, layers) in enumerate(sorted(SHOTS.items())):
        n = int(SR * DUR)
        left, right = [0.0] * n, [0.0] * n
        for name, gen in layers:
            # 层内左右声道用相邻种子去相关，层间顺序叠加
            for sign, dest in ((0, left), (1, right)):
                src = gen(SR, DUR, random.Random(seed0 + idx * 100 + sign * 7 + hash(name) % 53))
                for i in range(n):
                    dest[i] += src[i]
        path = os.path.join(out_dir, f"{shot}.sfx.wav")
        lvl = 0.16 * SHOT_LEVEL.get(shot, 1.0)
        write_stereo(path, normalize(left, lvl), normalize(right, lvl))
        print(f"  {shot}.sfx.wav  ({DUR:.0f}s)")

if __name__ == "__main__":
    main()
