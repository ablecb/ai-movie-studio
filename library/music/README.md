# 本地配乐曲库

把**免版权**的配乐素材放进本目录，`studio deliver` 会按 `music.json` 里每条 cue 的
情绪/配器/时长自动选曲，复制为 `output/music/<cue_id>.<ext>` 并对齐到所属场景的时间段。
曲库为空时不会静默出干片：交付报告会逐条 cue 告警"缺配乐素材"。

## 两种供给方式

**方式一：文件名打标签（零配置，推荐起步）**

文件名用下划线/空格分隔标签，BPM 写成 `60bpm`：

```
悬疑_克制_60bpm_低音弦乐.wav
温暖_上扬_90bpm_弦乐群.wav
紧张_脉冲_120bpm_电子.wav
```

匹配规则：cue 的 mood/section/instruments 与标签做包含命中（每命中 +2 分），
BPM 相差 ±10 以内加分，时长够用加分——**分数为 0 宁可不选**，不会乱配。

**方式二：index.json（标签更准，适合整理过的曲库）**

```json
{
  "items": [
    {"file": "tracks/ocean-ambient.wav", "tags": ["辽阔", "弦乐", "氛围"], "tempo": 60, "duration_sec": 183.2}
  ]
}
```

`file` 相对本目录；`tags` 命中规则同上；给了 `index.json` 就不再扫描文件名。

## cue 如何对齐场景

`music.json` 的每条 cue 带 `scene_id`（引用分镜的场景）。deliver 会把选中的素材
对齐到该场景的实测时间槽（起点=场景首镜实测入点，终点=末镜实测出点），
素材超长会在场景结束处截断。**请为不同情绪段落准备不同的素材**，
让"悬疑→上扬"这类音乐转折正好落在场景切换上。

## 生成 API（可选）

不想整理曲库时，可在 config.json 开启：

```jsonc
"music":   { "library_dir": "library/music", "generate": true },
"providers": {
  "music": { "base_url": "https://api.minimax.chat", "api_key_env": "MINIMAX_API_KEY", "model": "music-01" }
}
```

曲库无匹配的 cue 会调用生成接口产出纯音乐；失败只告警，不阻断交付。
