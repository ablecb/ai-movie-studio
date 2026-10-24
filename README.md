# studio —— 专业级 AI 电影制作流水线（Go）

把"一个想法"变成**一部能交付的成片**：角色跨镜头同一张脸、台词与配音严丝合缝、
字幕跟片走且可读、声轨达到流媒体响度标准、缺镜头时拒绝出片。

```
brief（创作意图）
  │
  ▼  studio run          创作与质检：剧本（结构化：目的/冲突/转折/幕）→ 角色圣经 → 素材库 → 分镜 → 生成规格 → 审片
  │                      素材阶段在分镜前产出一致性素材（角色参考帧 + 场景图），渲染直接复用
  │                      产出 shotspec.json（台词/画面/摄影参数的唯一真源）
  ▼  studio render       逐镜头生成：角色参考帧锁脸 → **参考帧先过质检**（对照圣经逐项核对，不合格重出）
  │  [--images-only]       → 参考图+定种出图 → 图生视频
  │                        → 客观质检（夜戏阈值自动放行、饱和度上限拦截糖水图）→ 缺陷镜头换种重渲
  ▼  studio animatic     动态分镜：用首帧 + 分镜时长拼灰盒预览——确认节奏后再为视频付大钱
  ▼  studio deliver      配音 → 字幕（按实测重排）→ 混音（两遍 loudnorm）→ 合成（镜头间曝光匹配
  │                        + 按 Style.Aspect 宽银幕裁切 letterbox + 叠化/淡入淡出）→ 成片校验
  ▼  studio polish       打磨闭环：视觉复核（身份/解剖/电影感分维度）→ 不及格镜头定点重渲 → 重交付，循环到达标
  ▼  studio verify       复核交付报告（完整性/时长/响度/字幕可读性逐项核对）
     output/final_release.mp4
```

---

## Web 控制台（按业务模块操作）

不想记命令时，用内置 Web 控制台在浏览器里操作整条流水线：

```bash
go build -o bin/studio ./cmd/studio
./bin/studio serve                # 默认 http://127.0.0.1:8787，自动打开浏览器
./bin/studio serve --addr 0.0.0.0:8787   # 需要局域网访问时
```

侧边栏按业务模块划分菜单：

| 模块 | 内容 |
|---|---|
| 🏠 总览 | 项目统计、开始一部新片（brief → 创作流水线，支持 `--until` 分段停下）、最近任务 |
| ✍️ 创作中心 | 工作流阶段流水线视图（含失败原因与修复建议）、剧本/角色圣经/分镜/台词等产物卡片视图、断点续跑 |
| 🖼️ 素材库 | 基于剧本生成一致性素材：角色参考帧（与渲染同源，图生图锁脸 + 圣经核对质检）与场景 establishing 图（场次美术基准）。**已是流水线正式阶段**（角色圣经后、分镜前自动生成）；页面左侧场景描述、右侧该场景用到的素材；支持按角色·场景定点重出 |
| 🎞️ 渲染生成 | 渲染操作台（成本预估 → 仅出首帧 → 完整渲染）、镜头缩略图网格（点开看视频/重渲历史/换种种子）、角色参考帧画廊与质检结论、定点重渲、多 Take 对比 |
| 🔊 声音字幕 | 配音台词逐句试听（实测时长/入点/溢出标记）、字幕预览、配乐 Cue Sheet、混音母版播放 |
| 📦 交付成片 | 交付选项（视觉复核/无字幕母版/允许缺镜）、成片在线播放与下载、交付报告逐项校验、水印检测结论 |
| 🔍 质检复核 | 打磨闭环（复核→定点重渲→重交付）、视觉复核分维度评分与逐镜头问题、复核抽帧画廊、隔离区、一键重渲不及格镜头 |
| 🗂️ 运行管理 | 全部项目列表（阶段/渲染/花费/模式）、Token 用量、任务历史与日志、断点续跑 |
| ⚙️ 系统设置 | ffmpeg 能力自检 + AI 诊断、内置风格预设、工作流定义、经验记忆库、配置查看（api_key 自动隐藏） |

**设计边界**：Web 层不重新实现流水线——所有付费/重活都以受控子进程调用 `studio` 的
各子命令执行，运行锁（同一 run 同时只允许一个任务）、断点续跑、预算熔断、失败留痕
对 Web 发起的任务原样生效。任务实时日志可看、可取消（进程树整树终止）。
API Key 永不出网：配置查看接口递归掩掉 `api_key` 字段；文件接口拒绝路径穿越与隐藏目录。

---

## 为什么它不像"AI 生成的"


AI 痕迹不是靠提示词里加 `cinematic, 8k, masterpiece` 消除的——那些词恰恰会触发
AI 学的生成模式（塑料皮肤、糖水 HDR、过度锐化）。本流水线用**工程手段**逐项消除：

| AI 痕迹 | 来源 | 本项目的处理 |
|---|---|---|
| 跨镜头换脸 | 纯文字描述无法锁定身份 | **角色参考帧先行**：先出一张标准像锁死身份，所有出镜镜头都以它做图生图 + 确定性种子 |
| 塑料皮肤 / 蜡像感 | 模型默认"美化" | 提示词强制正向要求 `visible pores / subsurface scattering / natural asymmetry`，负面固化 `plastic skin / wax figure / poreless skin` |
| 无来源的均匀补光 | 提示词没写照明动机 | `light_key` 必须是**画面内可见光源**（控制台辉光/窗户/台灯），缺失直接判定校验失败 |
| 多余手指 / 畸形手 | 手部生成是模型弱项 | 负面按 `anatomy` 分组固化；构图明确 `no crop on face or hands`；视觉复核专项检查 |
| 镜头运动"飘" | 运动提示词没约束 | `video_prompt` 强制写入 `physically plausible / identity stay locked / no morphing / no speed ramping`；首尾帧夹逼把运动终点钉在分镜构图上（`end_frame_anchor`） |
| 构图不像电影 | 未指定光学参数 | 按景别映射真实镜头：大远景 24mm T2.8 → 特写 85mm T1.8，含景深/眩光/胶片型号 |
| 画面糊/纯色/过曝 | 生成偶发失败 | **客观质检**用信号处理判定（拉普拉斯方差测清晰度、亮度标准差测纯色、肤色连通域测人脸占比），不合格自动换种重渲 |
| 糖水片质感（重散景+青橙过饱和） | 模型默认审美 | 负面词组 `look` 固化（heavy bokeh / teal-orange / beauty filter）+ 客观质检**饱和度上限**（夜戏放宽 0.10）双道拦截 |
| 跨镜头服装道具漂移 | 模型不知道上一镜 | shotspec 的 `continuity` 衔接说明注入每镜提示词（视线/轴线/服装状态），此前该字段只落盘不消费 |
| 同场戏环境漂移（"换了屋子"） | 每镜独立出图，环境只靠文字描述 | 场次 establishing 基准图**逐镜绑定**（素材库 `assets/scenes`，状态变体经 shotspec 的 `scene_state` 精确命中），出图时与角色参考帧一起喂多参考图模型；视觉复核同步比对同一张基准（`scene` 维度） |
| 口型对不上台词 | 生成端无声生成、配音后期叠加——两者之间没有任何绑定 | ① 台词原文 + 镜头内时间窗 + 说话人写入 `video_prompt`（"说话"至少有一句话的起止节奏）；② `mouth_visibility` 声明嘴部可见性：**visible 不允许搭配特写/大特写**（正对嘴部是穿帮的最坏形态），台词改由听者反应/过肩/侧脸/前景遮挡/出画等"看不见嘴"的构图承载——与真人电影的正反打语言一致 |
| 宽银幕感靠嘴说 | 画幅只是提示词 | 交付时按 `Style.Aspect`（2.39:1）中心裁切 + letterbox 进容器——HD scope 标准交付，字幕落在下黑边 |
| 风格每镜都变 | 每镜独立描述 | 全局 `Style` 预设（机型/胶片/调色/颗粒/光晕）由代码固化，全片统一，逐镜只做变奏 |
| 平台水印角标 | 生成接口默认加水印 | 出图/视频请求**显式**传 `watermark_enabled: false`（默认 `true` = 加显式+隐式水印）；见下文「水印」 |

风格由代码固化，可用 `studio presets` 查看；brief 或剧本里的风格关键词会自动命中预设。

---

## 为什么台词与字幕对得上

这是**结构**问题，不是参数问题。做法是把"台词的唯一真源"钉死，并全程用**实测值**：

1. **单一真源**：`voiceover.json` 是台词的唯一来源。`shotspec` 把每一句台词绑定到
   具体镜头的具体节拍（`beats[]`），配音、字幕、剪辑全部读这一份绑定结果。
   → 校验会逐句核对"台本里的每句话是否都被绑定"，漏绑直接失败。

2. **实测优先**：每个镜头用 ffprobe 实测时长；每句台词单独 TTS 后实测音频时长；
   字幕入点 = 实测人声起点，字幕时长 = 实测人声时长 + 余量。
   → 不用"中文字数 ÷ 4"的估算（估算偏差会累积成可见的音画不同步）。

3. **不越镜**：字幕一律夹在所属镜头的时间槽内；排版后再逐条校验
   （最短显示时长、每秒字符数 CPS、行数、行宽、重叠），有问题显式报出。

4. **可读性**：中文单行最多 18 字、最多 2 行，标点优先断行；CPS 上限 9 字/秒
   （超了就延长显示时间，而不是让观众读不完）。

5. **成片校验**：交付时核对"最后一条字幕是否超出成片时长""字幕是否越出所属镜头"
   "响度是否达标""峰值是否削波""成片时长是否等于镜头实测之和"，全部写进交付报告。

---

## 快速开始

```bash
go build -o bin/studio ./cmd/studio

# 1) 环境与能力自检（先确认工具链，再花钱）
./bin/studio doctor

# 2) 创作与质检（无 API Key 时自动进入离线演示模式）
./bin/studio run --brief "一部 2 分钟科幻短片：深空信使收到来自地球麦田的神秘信号，踏上归途。"

# 3) 成本预估 + 计划自检 + 示例提示词（不花一分钱）
./bin/studio render --run runs/<id> --dry-run

# 4) （可选但推荐）动态分镜：只出首帧图，拼成灰盒预览确认节奏与叙事
./bin/studio render --run runs/<id> --images-only   # 首帧就绪后自动拼 animatic.mp4
./bin/studio animatic --run runs/<id>               # 首帧已在时单独重拼

# 5) 试点：先渲 2 个关键镜头验证画质与成本
./bin/studio render --run runs/<id> --max-shots 2

# 6) 全量生成（断点续跑，只补失败镜头；--images-only 的首帧会被直接复用）
export AI_API_KEY=<你的智谱 Key>
./bin/studio render --run runs/<id> --workers 2

# 7) 交付：配音 + 字幕 + 混音 + 合成 + 成片校验
./bin/studio deliver --run runs/<id>

# 8) 复核
./bin/studio verify --run runs/<id> --render
```

### 常用命令

| 命令 | 作用 |
|---|---|
| `studio run --brief "…" [--until script]` | 创作流水线；`--until` 分段确认，最省钱的用法 |
| `studio render --run <id> [--dry-run]` | 逐镜头生成；`--dry-run` 只预估不花钱 |
| `studio render --run <id> --images-only` | 只生成参考帧+首帧图（几厘钱级），随后自动拼 animatic |
| `studio animatic --run <id>` | 拼动态分镜（灰盒预览）：节奏不成立就不该为视频付钱 |
| `studio render --run <id> --shots sh003,sh007` | **定点重渲**指定镜头（最小成本返修） |
| `studio render --run <id> --force-ref` | 重新生成角色参考帧（会改变全片长相，慎用） |
| `studio deliver --run <id>` | 配音/字幕/混音/合成/校验一条龙 |
| `studio deliver --run <id> --visual` | 额外做视觉复核（模型看画面，给重渲清单） |
| `studio polish --run <id> [--rounds 2]` | **打磨闭环**：视觉复核 → 不及格镜头定点重渲 → 重交付，循环到达标（预算熔断全程生效） |
| `studio takes --run <id> --shots sh003` | 关键镜头**多 take 对比**：备份旧 take → 换种重渲新 take → 并排对比片 + 机器评分；`--pick B` 才切换 |
| `studio deliver --run <id> --no-burn` | 只出无字幕母版 + 外挂 SRT |
| `studio assets --run <id>` | **素材库**：生成角色参考帧 + 场景 establishing 图（一致性素材）；`--chars 名` / `--scenes s1` 定点重出，`--force` 强制 |
| `studio verify --run <id>` | 查看交付报告（每项校验的实测值与判定） |
| `studio verify --run <id> --render` | 对已生成镜头复跑客观质检 |
| `studio doctor [--run <id>]` | 环境能力自检 + 失败阶段 AI 诊断 |
| `studio resume --run <id>` | 断点续跑（已付费的产物不会重买） |
| `studio presets` / `studio lessons` | 查看风格预设 / 经验记忆库 |

---

## 产物结构

```
runs/run-xxx/
├── state.json                   # 阶段状态 + token 用量（原子写；.lock 运行锁防并发操作）
├── artifacts/
│   ├── script.json characters.json storyboard.json prompts.json
│   ├── voiceover.json           # 台词唯一真源
│   ├── shotspec.json            # ★ 生成规格：镜头参数/一致性锚点/台词绑定
│   ├── music.json subtitles.json editplan.json review.json
│   └── narration.json           # 配音计划（逐句实测时长与入点，含 J-cut 提前量）
└── output/
    ├── refs/                    # ★ 角色参考帧（全片一致性的基准；质检结论随 render_manifest 落盘）
    ├── shotplan.json            # ★ 逐镜头生成计划（提示词/种子/参考图）
    ├── shots/                   # 镜头片段；invalid/ 隔离不合格产物（保留证据）
    ├── takes/                   # 多 take 对比（takeA/takeB/compare.mp4/评分 JSON）
    ├── render_manifest.json     # ★ 渲染清单：每镜头的每轮尝试、指标、缺陷、参考帧质检结论
    ├── audio/                   # 逐句台词音频（raw 与处理后的）
    ├── audio_mix.wav            # 混音母版（48kHz）
    ├── subtitles.srt            # 按实测重排的字幕
    ├── final_cut.mp4            # 无音轨母版（按 Style.Aspect 宽银幕 letterbox）
    ├── final_with_audio.mp4     # 有声母版
    ├── final_release.mp4        # 发行版
    ├── review_frames/           # 视觉复核抽帧（可人工核对模型判断）
    ├── delivery_report.json     # ★ 交付报告（所有校验的实测值 + visual_qa 分维度评分）
    ├── subtitle_violations.json # 字幕排版问题（若有）
    ├── render.sh                # 一键跑完 render → deliver → verify（bash）
    └── render.ps1               # 同上（PowerShell，Windows 用户用这个）
```

---

## 配音音色（一个角色一个声音）

"一开口就知道是谁"由流水线自动保证，不靠每部片子手工配：

1. **角色圣经阶段选音色**：音色池默认 `tongtong / chuichui / xiaochen / jam`
   （智谱 GLM-TTS 官方 voice 枚举，见 [API 文档](https://docs.bigmodel.cn/api-reference/模型-api/文本转语音)；
   还有 `kazi / douji / luodo` 三个动物圈音色可用）。模型按角色性别/年龄/气质挑选，
   代码做**确定性去重**：缺失/重复/越池的按池序补齐，`tongtong`（旁白默认音色）排最后分配，
   保证角色与旁白天然区分；音色池不够时清空回落并显式告警（narrate 层用变调兜底区分）。
2. **逐句语气**：每句台词带情绪标注（平静/开心/悲伤/愤怒/恐惧/惊讶/厌恶/低语）与
   0.5–2 的逐句语速（急促 1.1、沉痛 0.9），语速由服务端真实合成，不是后期拉伸。
   注意：glm-tts 无 emotion 参数（靠标点韵律兜底）；情绪参数要真正生效需把
   `providers.tts` 指到 MiniMax 等（`MapEmotion` 已做好映射）。
3. **可覆盖**：单个角色用 `config.audio.voice_map`（角色名→音色名）强制指定；
   整池用工作流参数 `voice_pool` 替换（`workflows/movie.json` 的 characters 阶段）。

配音开工时会打印**音色表**（`🎙 音色表：narrator=tongtong，信使·林=chuichui，…`），
两个角色撞音色在这里一眼可见。

## 配乐与音效（把方案变成声音）

`music.json` 产出的 Cue Sheet 与拟音清单**会被后期真正消费**——不是写完就扔的方案。
系统按约定匹配磁盘上的素材：

```
output/music/cue01.wav      按 music.json 的 cue 铺在时间轴上（文件名 = cue id）
output/music/1.wav          也接受顺序号命名
output/sfx/sh003.sfx.wav    挂在对应镜头起点（文件名 = <shot_id>.sfx.<ext>）
```

**cue 精确对齐场景边界。** music 技能给每条 cue 强制绑定 `scene_id`（引用分镜的场景，
绑错直接产出校验失败）。deliver 按该场景的**实测镜头槽**把配乐对齐到场景起止：
起点 = 场景首镜实测入点，终点 = 末镜实测出点；素材超长会在场景结束处截断，
不会漫进下一条 cue 叠成"同时响两首"。情绪段与画面段因此重合，
"悬疑→上扬"的音乐转折正好落在场景切换上。旧方案文件（cue 无 `scene_id`）
自动退回从 0 顺序铺，不会因此丢配乐。

曲库自动供给见 [`library/music/README.md`](library/music/README.md)：
按文件名情绪标签选曲（`悬疑_克制_60bpm_弦乐.wav`），或整理 `index.json`；
曲库无匹配且配置了生成端点时按 cue 描述生成纯音乐。
**配乐缺口会写进交付报告**（不只是终端一闪而过），缺哪条 cue、往哪放素材，逐条列明。

补齐素材后重跑 `studio deliver` 即可，无需改任何配置：

```
🔊 混音完成：-14.0 LUFS / 峰值 -11.7 dBFS（人声 4，配乐 2，音效 1）
  声轨: -14.0 LUFS / 峰值 -11.6 dBFS（4 句台词，2 配乐轨，1 音效）
```

**配乐自动闪避人声**（侧链压缩）：对白一出现，BGM 自动让路约 10dB，
对白结束后恢复——这是行业做法，比"把 BGM 整体调小"更能同时保住对白清晰度与配乐存在感。
实测（`TestSidechainDuckingActuallyLowersMusic`）：人声段配乐 **-30.1 → -40.5 dB**。

**对白总线轻压缩**（`audio.voice_compress`，默认开）：TTS 逐句合成的电平天然不一致，
同一角色忽大忽小。轻参数（3:1、低阈值、慢启动）只稳住底部，不压平表演起伏。

**配乐在场景边界淡出**（`audio.cue_fade_out_sec`，默认 2s）：戛然而止的配乐是
"AI 味"的听觉特征之一；淡出让"悬疑→上扬"的音乐转折落在切换点上而不是撞在切换点上。

**J-cut**（`audio.jcut_sec`，默认 0.3s）：前一镜完全无台词时，下一镜首句台词的声音
提前越过硬切点——电影最常用的转场语法（"声音带着观众进下一镜"）。字幕与音频按同一
提前量排版，音画字保持同步；与台词/槽位冲突时自动退化为普通排麦，绝不叠音。

音效文件名必须带 `.sfx` 标记。这是刻意的：若用 `<shot_id>.wav`，
会与按 cue 命名的配乐（`cue01.wav`）撞名，把配乐误挂成某个镜头的音效——
成片里表现为"某个镜头突然响了不对劲的声"，极难排查。

**音效曲库自动匹配**：`library/sfx/` 下按描述标签命名的音频（`雨_屋顶_循环.wav`）
会按 music.json 的 sound_fx 描述自动匹配，复制为 `output/sfx/<shot_id>.sfx.wav` 挂到镜头；
未命中的音效在交付报告里逐条列明缺什么、往哪放。

没有素材时不会假装有：`deliver` 明确报告"本次成片无配乐"，而不是静默出一部干片。

---

## 多参考图出图（双角色同框的图像级约束，可选）

单参考帧图生图只能锁"一张脸"：两人同框时第二个人没有图像级约束。
`providers.image_edit` 把**全部出镜角色的参考帧**一起喂给支持多图参考的图像编辑模型
（火山方舟 Seedream / 阿里百炼 Qwen-Image-Edit），直接产出"合影式"首帧：

```jsonc
"providers": {
  "image_edit": { "base_url": "https://ark.cn-beijing.volces.com", "api_key_env": "ARK_API_KEY", "model": "doubao-seedream-4-0-250828" }
}
```

- **严格模式**：留空 = 显式走单参考帧路径；一旦配置了端点，渲染启动时端点/密钥必须
  可用——配置不完整直接**拒绝开工**（不允许"配了等于没配"、双角色静默失去图像级约束）。
- 该路径失败即镜头失败（由重试/中止处理），不回落单参考帧；走通时模型标识记为 `edit:<model>`。
- 智谱没有多图参考出图接口，该能力的端点指向火山方舟/阿里百炼。**需在 `config.local.json`
  配置**（仓库默认 config.json 不预置——它依赖本机专属密钥）；配 `ARK_API_KEY` 即启用。
- **能力路由**：`providers.<能力>` 覆盖各自端点——文本（创作/规划/翻译）、视觉（复核/质检）、
  图像、视频、TTS、多参考图（image_edit）、音乐各自独立、可指向不同供应商，共用同一花费账本。
  严格模式：独立端点缺密钥**直接报错**（不会静默继承顶层密钥、也不会退回离线演示）；
  想显式关闭多参考图就把 `providers.image_edit` 留空。

## 多模态参考出视频（Seedance 2.x 全模态参考）

首尾帧夹逼把"运动终点"钉死，但视频模型只看到首尾两张图——角色的标准像、场景的美术基准
都进不了视频请求。Seedance 2.x 的**全模态参考**任务解开了这个限制：一次上传该镜头相关的
全部参考照片，并在提示词里**逐张声明用途**（官方 `@图像N` / `@imageN` 语法）。

```jsonc
// config.json：reference_model 配成 Seedance 2.x 即启用（留空退回首尾帧夹逼模式）
"video": { "reference_model": "doubao-seedance-2-0-260128" }   // 2.5 = doubao-seedance-2-5-260628（30s/50 参考/10bit）
```

每个镜头发出的参考图与声明（顺序即语义，编号即位置）：

1. **出镜角色标准像**（身份锚）——`@image1 is the standard portrait of 杨戬 … identity stays locked`
2. **场次 establishing 基准**（环境锚）——`@image2 is the establishing baseline of this scene's location …`
3. **本镜已定稿首帧**（构图锚）——`@image3 is the approved opening frame of this shot — begin the video from this exact framing`

工程保障：

- **任务类型显式声明**：请求带 `omni_reference_task_type=reference`——不写的话模型按提示词
  自判，误判成"编辑/延长"（对 ratio/duration 有硬约束）会异步报错；显式声明还把参数校验
  前置成同步报错，不白等一轮排队。
- **与首尾帧互斥**：参考任务不发首/尾帧（官方约束），`end_frame_anchor` 的锚点出图自动跳过、
  不为用不上的图付费；构图控制由第 3 条"首帧参考图声明"承担。链式镜头只有在首帧真的
  需要生成时才优先走首尾帧路径（首帧已在盘上时，复用的定稿首帧直接当构图锚）。
- **时长/分辨率照常下发**：`videoDurationFor` 的 5/10s 档位与 `video.size` 的分辨率档
  同样作用于参考任务（2.0 支持到 15s，2.5 支持到 30s），交付重排不会对不上分镜节奏。
- **2.x 差异由代码适配**：`generate_audio` 显式关掉（2.x 默认生成音轨，本项目自己做后期）；
  首尾帧任务（`video.model` 指 2.x 时）首帧显式标 `role=first_frame` 且 `ratio` 强制 `adaptive`。
- **开通与计费**：Seedance 2.x 需在方舟控制台开通（账户余额 > 200 元或购买资源包）；
  单价与 1.0 pro 不同，启用后请按控制台报价校准 `pricing.video_per_sec`，否则预算预估失真。

## 视觉复核（让模型真的看画面）

文本审片永远发现不了 AI 电影真正的破绽——**跨镜头换脸、多余手指、塑料皮肤、运动形变**。
这些只有看着画面才能判断。技术质检（`quality` 包）负责确定性缺陷（糊/黑/纯色/人脸畸大），
视觉复核负责**语义缺陷**："这还是同一个人吗""像不像真实拍摄"。

### 参考帧质检（一致性之根，渲染前执行）

参考帧是全片一致性的地基——参考帧一旦与角色圣经不符（真实案例：圣经写明圆框眼镜、
参考帧没画眼镜、成片里眼镜忽有忽无），错误会经图生图放大到**每一个出镜镜头**，
事后重渲的不是一帧而是一部片。因此渲染开工前：

1. 参考帧出图提示词把标志性特征升级为 **MUST 级约束**（`Identity anchors that MUST be clearly visible`）；
2. 出图后由视觉模型对照角色圣经**逐项核对**（发型/五官/眼镜/疤痕/服装/皮肤），
   产出 `missing`（圣经有画面没有）与 `drift`（画面有圣经没有，AI 自作主张加戏）；
3. 不合格自动**换种重出**（`ref_qc.max_rounds`，默认 2 轮，每轮换种子 + 追加修正指令）；
4. 结论随 `render_manifest.json` 落盘：`pass / corrected`——严格模式下**没有"跳过"**：
   无视觉密钥或探活失败时**拒绝开工**；重出轮数用尽仍不合格也直接失败
   （修正角色圣经后 `--force-ref` 重跑）。

```jsonc
"ref_qc": { "mode": "strict", "max_rounds": 2 }   // off = 关闭（不建议）
```

failed（重出预算耗尽仍不合格）时保留最佳候选并告警——这时该修的是**角色圣经**，
然后 `--force-ref` 重出，而不是硬着头皮渲染。

### 分维度打分：身份一致性一票否决

复核分数从单一均分升级为**三个维度**（0-10）：

| 维度 | 判什么 | 为什么单独拆 |
|---|---|---|
| `identity` 身份一致性 | 与标准像/跨镜头是否"就是同一个人" | 均分会把"身份 3 分+电影感 9 分"平均成 6 分的灰色地带——换脸不可原谅，总分再高也不算过 |
| `anatomy` 解剖与形变 | 手/肢体/牙齿/面部畸变 | 生成弱项，独立阈值 |
| `cinematic` 真实感/电影感 | 皮肤质感、光影动机、构图 | 观感项 |

报告附各维度均分（`identity_avg` 等），并给出**校准线索**：身份分显著垫底时，
提示优先核对参考帧链路（`--force-ref`）而不是逐镜头盲目重渲。

```bash
# 按配置启用（config.vision_review.enabled = true），或命令行临时开启
studio deliver --run runs/<id> --visual
studio deliver --run runs/<id> --no-visual     # 临时跳过
```

### 视觉模型必须支持图片输入

`models.vision` 配错时，`deliver` 的探活会失败并**直接报错停止**——
不再"以为查过了、其实没查"，而水印/换脸恰恰只能靠这一步发现。
因此项目在**配置自检**里就把它拦住（开工前，而不是交付时）：

```jsonc
"models": { "vision": "glm-5.3-flash" }   // 智谱：从下面白名单里选
```

白名单按服务商分家（视觉复核常与画面生成不是同一家）：

| 端点 | 可用视觉模型 |
|---|---|
| 智谱 | `glm-5.3-flashx` `glm-5.3-flash` `glm-5v-turbo` `glm-4.6v` `glm-4.6v-flash` `glm-4.6v-flashx` `glm-4v-flash` `glm-4.1v-thinking-flashx` `glm-4.1v-thinking-flash` `autoglm-phone` |
| DeepSeek | `deepseek-flash`（v4-pro 不支持 Vision） |

⚠️ **旧值 `glm-4v-plus` / `glm-4v` 已不在智谱支持列表内**，沿用会让复核直接失败。
自检发现时会直接报错并列出可选值，不会让你带着一个不生效的复核跑完全片。

它做的事：从每个镜头抽 3 帧（避开首尾 8%，那里常有过渡伪影）→ 缩到 640px →
**同时附上出镜角色的标准像**（`output/refs/`，复核请求里标准像在前、镜头帧在后，
并逐张声明"第 N 张图 = 某角色的标准像"）→ 按镜头分组送视觉模型 → 逐镜头打分并列出具体问题 →
产出**重渲清单**。"是不是同一个人"由此从跨镜头印象变成与确定性基准的逐人比对；
参考帧不可用（旧 run / 缺产物）时自动退化为跨镜头对比，并在报告 Notes 里显式说明。

```
👁 视觉复核：4 个镜头均分 7.9（模型 deepseek-flash）
   ⚠ sh002 5.2 分：右手出现六根手指；面部与参考角色不一致
   建议定点重渲: studio render --run runs/<id> --shots sh002
```

复核结论写入 `delivery_report.json` 的 `visual_qa`，并作为一项交付校验展示。
**它不阻断交付**——画面语义是模型的主观结论，一次误判不该卡死生产；但结论与重渲清单
会明确摆出来让你决定。抽帧文件保留在 `output/review_frames/`，可人工核对模型判断是否可信。

### polish：把"建议重渲"变成"已经重渲完再看一遍"

复核给出重渲清单后，流程过去就停了——闭环的最后一步（真正去重渲）靠人手动执行。
`studio polish` 把这一步接上：

```
🔍 视觉复核（身份/解剖/电影感分维度） → 不及格镜头定点重渲（换种，机制与 --shots 相同）
   → 重新交付（字幕/配音按新镜头实测时长重排） → 再复核 …… 循环到达标、轮数用尽或预算触顶
```

- 每轮账目透明：复核花视觉调用的钱，重渲只花不及格镜头的视频钱，交付是本地 ffmpeg
- 任何一轮中断都保留断点：已重渲成功的镜头不会被重买
- 收尾自动做一次带视觉复核的完整交付，保证 `delivery_report.json` 描述的就是当前成片
- 若"身份一致性均分"显著低于"电影感均分"，polish 会先提示查参考帧——问题多半在根上

### takes：关键镜头多条选一

电影工业"一条过"是例外。`studio takes --shots sh003` 对指定镜头：

1. 现有 take 备份为 `output/takes/sh003.takeA.mp4`（**绝不丢已付费资产**）；
2. 换种重渲产出 takeB（与定点重渲同机制：换种子 + 提示词变体）；
3. 拼接并排对比片（`sh003.compare.mp4`）+ 视觉模型对两个 take 打分给出推荐；
4. **默认恢复 takeA**——对比流程不得悄悄改变成片现状；`--pick B` 显式切换
   （切换后镜头时长可能变化，需重跑 `studio deliver` 重排时间轴）。

### 用哪家的模型做复核

任何**支持图片输入**的 OpenAI 兼容服务都可以。项目支持**按能力分别配置服务商**，
因为生成端和复核端常常不是同一家：

```jsonc
// config.deepseek.json：画面生成走智谱，文本与复核走 DeepSeek
"models":  { "vision": "deepseek-flash", "text": "deepseek-flash" },
"providers": {
  "vision": { "base_url": "https://api.deepseek.com", "api_key_env": "DEEPSEEK_API_KEY" },
  "image":  { "base_url": "https://open.bigmodel.cn/api/paas/v4", "api_key_env": "AI_API_KEY" },
  "video":  { "base_url": "https://open.bigmodel.cn/api/paas/v4", "api_key_env": "AI_API_KEY" }
}
```

```bash
export DEEPSEEK_API_KEY=...   # 文本与复核
export AI_API_KEY=...         # 画面生成
studio deliver --run runs/<id> --config config.deepseek.json
```

**DeepSeek 注意事项**（依官方文档）：

- 必须用 **`deepseek-flash`**（DeepSeek-V4.1-Flash）——它支持图片输入；
  同系列的 `deepseek-v4-pro` **不支持 Vision**，配错会在复核时才失败
- 图片**只能出现在 user 消息**中，system 带图会返回 400（本项目已按此实现）
- 单请求最多 600 张图 / 总 64 MiB；单图自动缩放到约 1300×1300，**每张最多 1024 token**
- 旧名 `deepseek-v4-flash-vision-exp` 仍可调用，但请求由最新 Flash 承接

**提前探活**：`deliver` 在复核前会发一张 1×1 图片验证该端点与模型真的支持视觉
（`ai.Client.Ping`）。配错模型会在**开工前**就报错停止（严格模式），而不是渲染花完钱之后
——确需跳过复核请显式 `--no-visual`。

**花费计入同一账本**：复核客户端与生成客户端共享花费账本，
因此视觉复核的花费同样受 `budget.max_cost_cny` 预算熔断约束——
不会出现"预算之外的付费出口"。

### 出图 / 出视频走火山方舟（Seedream / Seedance）

默认配置里**出图与出视频已指向火山方舟**（`providers.image` / `providers.video`），
模型分别是 Seedream 出图与 Seedance 出视频；文本/视觉/TTS 仍走顶层（智谱）。
两家的协议差异由代码自动适配，配置只改端点与模型名：

```jsonc
"providers": {
  "image": { "base_url": "https://ark.cn-beijing.volces.com/api/v3", "api_key_env": "ARK_API_KEY", "model": "doubao-seedream-4-0-250828" },
  "video": { "base_url": "https://ark.cn-beijing.volces.com/api/v3", "api_key_env": "ARK_API_KEY", "model": "doubao-seedance-1-0-pro-250528" },
  "image_edit": { "base_url": "https://ark.cn-beijing.volces.com/api/v3", "api_key_env": "ARK_API_KEY", "model": "doubao-seedream-4-0-250828" }
},
// base_url 写不写 /api/v3 都兼容（少了会自动补；漏了前缀会得到"空响应体"404，已修）
"image": { "model": "doubao-seedream-4-0-250828", "size": "2048x1152", "ref_size": "1024x1024" },
"video": { "model": "doubao-seedance-1-0-pro-250528", "size": "1920x1080", "reference_model": "" }
```

代码自动处理的差异（无需人工干预）：

| 能力 | 智谱（原路径） | 火山方舟（自动切换） |
|---|---|---|
| 出图 | `quality`/`ref_strength`/`watermark_enabled` | `watermark`（默认 true，显式写 false 去水印）；无 `ref_strength`；`size` 自动翻译到合法区间（总像素 ≥ 92.16 万，1248x704 → 2496x1408），画幅保持不变 |
| 出视频 | `POST /videos/generations` + `/async-result` 轮询 | `POST /api/v3/contents/generations/tasks` + 任务轮询；参数为顶层 JSON 字段（`ratio`/`resolution`/`duration`）；首帧=无 role 的 `image_url`（2.x 系显式 `role=first_frame`）、尾帧=`role=last_frame`、参考图=`role=reference_image`；2.x 首尾帧任务 `ratio` 自动改为 `adaptive`（官方约束）、2.x 参考任务显式发 `omni_reference_task_type=reference`；`video.size` 1920x1080 → `1080p` + `16:9` |
| 运镜 | vidu 系 `movement_amplitude` | Seedance `camera_fixed`（分镜"固定"→ 钉死机位） |

注意事项：

- **`ARK_API_KEY`**：放在 `config.local.json` 的 `providers.*.api_key`（或环境变量/
  `ARK_API_KEY`）。严格模式：缺密钥会在开工前直接报错，不会静默退回智谱或离线。
- **模型要先在方舟控制台开通**：未开通时服务端把"未开通/无权限"和"模型名写错"合并成同一句
  `InvalidEndpointOrModel.NotFound`——代码会补一条可执行提示（去「开通管理」开通，或把
  推理接入点 ID `ep-xxxxxxxx` 填进模型字段）。
- **计费要校准**：`pricing.image_per_shot` / `pricing.video_per_sec` 是预算熔断的依据，
  请按方舟实际报价填写——否则"预估成本/预算上限"会失真（Seedance 1080p 明显贵于
  CogVideoX，`budget.max_cost_cny` 也要相应提高）。
- **成本档位**：视频分辨率由 `video.size` 推导（1920x1080 → 1080p 收费档；
  改 `1280x720` 即 720p，便宜得多）；`--quality speed` 用 `video.economy_model`。
- **多角色同框**：`video.reference_model` 配成 Seedance 2.x 模型（如
  `doubao-seedance-2-0-260128`，需先在方舟「开通管理」开通）即启用**多模态参考出视频**
  （见下文）；留空则一致性由 `providers.image_edit`（Seedream 多图参考出首帧）+
  首尾帧夹逼保证。
- **模型自带音频**：2.x 系 Seedance 默认生成音轨，本项目自己做后期，会在请求里
  显式关掉（1.x 不发该字段——未知字段会 400）。

控制成本的旋钮（`config.vision_review`）：

| 参数 | 默认 | 作用 |
|---|---|---|
| `sample_frames` | 3 | 每镜头抽几帧（1-6） |
| `frame_width` | 640 | 抽帧宽度，越小越省 token |
| `max_shots_per_request` | 4 | 每次请求送检几个镜头 |
| `max_review_shots` | 0 | 最多复核多少镜头（0=全部）；长片可抽样，优先含角色的镜头 |
| `pass_score` | 7.5 | 单镜头达标分 |

---

## 水印（默认关闭）

出图与视频请求都会**显式**带上 `watermark_enabled: false`。这一步不能省：官方 spec 里该字段
**默认 `true`**（"显式水印 + 隐式数字水印"），也就是说"不传"等于全片带角标——而参考帧一旦带水印，
会作为参考图/首帧一路传下去，在成片里变成运动鬼影，不只是角上多一个标。

```jsonc
"image": { "quality": "hd", "watermark": false },
"video": { "watermark": false }
```

**两个前提**：

1. 需先在智谱控制台签署免责声明（**个人中心 → 安全管理 → 去水印管理**），
   否则该参数不生效。`studio doctor` 会提示这一点。
2. `watermark_enabled: false` 只关平台侧水印；**隐式数字水印与文件元数据里的生成标记
   不受该参数控制**。另据第三方逐帧 OCR 实测（2025-09 标识规定后），国产图片模型普遍带
   可见 `AI生成` 角标，视频则按厂商分化——所以别依赖任何"无可见水印"的说法，
   **交付前自己抽帧核对**。

`studio doctor` 会把水印状态打进生效配置摘要（`水印=图:去水印 视频:去水印`），
因为这件事取决于服务端参数与账号侧声明两处，看代码无法判断线上到底有没有角标。

### 交付时的水印复核

`deliver` 会把它作为一项校验写进 `delivery_report.json`（`画面无水印/叠字`），
由视觉复核顺手完成——它本来就在看帧，只需逐镜头额外回答一次"四角/下缘有没有叠字角标"。

**为什么不做成"确定性检测"**：试过了，不可行。合成对照实验（有角标 vs 无角标，
右下角区域的帧间时间变化 / 整帧帧间变化）得到 **0.77 vs 1.04**；而真实画面里一块静止的
墙角、台灯、窗外景色，这个比值接近 0——**在数值上和烧进去的角标完全重叠**。
区分"叠加的图形"与"画面里本来就有的静止物体"需要语义理解，只能由模型承担。

三种结论都写进报告，且**"没查"绝不表现为"通过"**：

| 情形 | 结论 | 动作 |
|---|---|---|
| 检出叠字（信心 ≥ 0.7） | ⚠ 告警 | 复核 `output/review_frames/`，确认声明与参数后定点重渲 |
| 确认无水印 | ✓ | 无需处理 |
| 未启用视觉复核 / 模型未答该字段 | ⚠ **未检查** | 加 `--visual` 重跑 |

它**不阻断交付**（画面语义是模型的主观判断，一次误判不该卡死生产），但会让终端的
"全部通过"变成"无阻断问题（N 项告警待人工确认）"——不让人误以为水印已被确认过。

---

## 交付门禁（不会产出"假成片"）

这是与 Demo 最本质的差别：**缺镜头时拒绝出片**。

`studio render` 有镜头失败时以非零退出码结束；`studio deliver` 在缺镜头时直接拒绝，
并给出补齐命令。确需内部审看残缺版才加 `--allow-incomplete`，且报告会显式标注。

交付前逐项校验（实测值写进 `delivery_report.json`）：

- 成片时长 == 各镜头实测时长之和（容差 0.35s）
- 镜头完整（0 缺失）
- 分辨率达标
- 响度达标（默认 -14 LUFS ± 1.5，流媒体标准）
- 峰值未削波（默认 ≤ -1 dBTP）
- 字幕条数非零 / 未超出成片时长 / CPS 可读 / 无阻断级排版问题 / 未越出所属镜头
- 画面无水印/叠字（需开启视觉复核；未检查会显式标为"未检查"而非通过）
- 台词未溢出镜头槽（溢出会顺延，不阻断但明确告警）

---

## 成本管控

生产环境最危险的是"跑着跑着把余额烧穿"。因此：

- **付费前预估**：`render --dry-run` 给出出图/视频/配音/文本的分项预估与合计
- **预算上限**：`config.budget.max_cost_cny`，超限立即停止付费调用并保留断点
- **用量告警**：达预算 `warn_ratio` 比例时告警
- **余额熔断**：识别"余额不足"错误后**停止整批**，而不是逐镜头重试白烧钱
- **媒体计费**：出图（张）、视频（秒）、TTS（字符）全部过账，不再只有 token 统计
- **定点重渲**：`--shots sh003` 只重跑指定镜头
- **断点续跑**：已存在的产物（含参考帧、台词音频、镜头片段）一律复用

注意事项：`config.json` 里的单价是**占位默认值**，请按你的实际计费改写；
不填也能跑，但成本统计会偏低。

---

## 环境要求

- **Go 1.22+**
- **ffmpeg / ffprobe**：推荐 4.1+ 完整版（[gyan.dev](https://www.gyan.dev/ffmpeg/builds/) essentials/full）
  - `loudnorm`（4.1+）用于响度归一化；**严格模式：缺失即报错**（混音直接失败，
    不再降级为增益校正），`doctor` 会提前告警
  - `ffprobe` 必须存在：媒体探测缺失即失败（不再降级为解析 ffmpeg 输出）
  - `subtitles` 用于字幕烧录；缺失时烧录会失败（显式 `--no-burn` 可只出无字幕母版 + 外挂 SRT）
  - 找不到时按顺序查找：环境变量 → PATH → 项目 `tools/` → 常见安装位置
    （含 Windows 上随软件附带的 ffmpeg）
  - 手工指定：`STUDIO_FFMPEG` / `STUDIO_FFPROBE`
- **中文字体**（字幕烧录）：默认 `Source Han Sans SC Medium`；
  严格模式：缺字体时字幕烧录直接失败（不再静默丢弃字体）
- **API Key**：`ARK_API_KEY`（火山方舟：Seedream 出图 / Seedance 出视频，默认走它）
  与 `AI_API_KEY`（智谱：文本创作 / 视觉复核 `glm-5.3-flash` / TTS）。
  未设置时进入离线演示模式：会用 ffmpeg 生成带噪点的占位素材，
  完整走通质检/字幕/混音/交付链路（便于无 Key 验证流程），但不是真实画面。
- **视觉复核启用即必须可用**：`vision_review.enabled=true`（或 `--visual`）时，
  端点/密钥/探活任一不可用 `deliver` 都会**直接报错**，不再静默跳过；
  确需跳过请显式 `--no-visual`（离线演示 mock 模式除外，会自动跳过）。

---

## 设计要点

**严格模式：不降级，出问题就阻塞。** 流水线不做任何静默降级——模型/端点不可用、
参考图读不出来、尾帧锚点生成失败、质检不过、缺 ffprobe / loudnorm / xfade、字幕烧录失败、
BGM 生成失败……一律直接报错停止（重试由镜头级/阶段级重试策略承担，AI 自我修复只有
"修复重试"与"中止"两种动作，没有"跳过"）。所有"要跳过"都必须是显式的：
`--no-visual`、`--no-burn`、`--skip-audio`、`transitions.enabled=false`、
`ref_qc.mode=off`、清空 `providers.image_edit` 等。离线演示（mock）除外——
它是显式的离线模式，不是降级。

**逐镜头生成，而不是整包生成。** 原实现让模型一次产出全部镜头的提示词，一次 JSON
解析失败就要重生成全部（实测一次调用 568 秒）。现在提示词按镜头编译并落盘
（`shotplan.json`），单镜头失败只重试那一镜头，且提示词可人工检查、可 diff。

**先 animatic 再"拍摄"。** 电影工业先用最便宜的手段看节奏，AI 电影里最贵的环节（视频按秒计费）
却常常第一个执行。`render --images-only` 用出图的小钱产出全部首帧（图片级质检照常），
`studio animatic` 按分镜时长拼成静帧预览片（有 deliver 产物时连台词 scratch 轨都铺上）。
后续完整渲染直接复用这些首帧，一分钱不浪费。

**叙事结构有机器下限。** "三幕结构"不能只是提示词里的一句话：剧本阶段每场必须声明
**purpose（本场为什么存在）、turn（转折）、act（幕号）与 tone（情绪基调）**，
全片无转折直接校验失败；声明了幕号则启用**节拍表校验**：幕序倒退（叙事断裂）直接失败，
整幕缺失直接失败，第二幕体量不足 20%、转折全部堆在第一幕、≥70% 场戏同一情绪基调、
相邻两场 purpose 雷同都会给出结构警告（节奏是创作判断，警告不重烧剧本）；
`time_of_day` 与动作描述的矛盾（夜场写"晨光"）用确定性规则拦截；
分镜阶段按 `duration_min` 换算镜头数区间与总时长预算（±55%~140% 门禁），
杜绝"brief 要 3 分钟、成片 87 秒"；运镜必须取枚举值（枚举外拿不到确定性镜头语言编译），
同景别连续 3 镜、单镜场景都会被判失败。

**模型给判断，代码给下限。** 模型只负责创作判断（景别/运镜/光位/情绪/角色细节），
摄影语言（焦段/光圈/胶片/调色/真实感约束/负面提示词）由 `promspec` 按固定结构编译。
这样"电影感"有确定下限，而不是每次运行靠运气。

**质检知道场景语境。** 阈值按日景标定，夜戏（time_of_day=夜）自动放行：
亮度下限放宽、黑帧占比上限放宽、跳过肤色启发式的人脸判定（暗光下不可靠）——
压黑是光比设计，不是生成失败；误判的代价是付费重渲且永远过不了。

**客观质检 + 主观复核分工。** 确定性缺陷（糊/黑/纯色/人脸畸大）用信号处理判定，
几十毫秒且可复现；主观判断（一致性/表演/叙事）留给模型复核。**审片不再只审文本**。
视觉复核"结论不可用"（请求失败/模型漏答）与"不合格"分开表达：前者不计分、不进重渲清单，
一次网络抖动不会被当成"画面不行"。

**失败必须留痕，且不能销毁好资产。** 不合格产物移入 `output/shots/invalid/` 而不是删除；
每个镜头的每一轮尝试、指标、缺陷都写进 `render_manifest.json`。换种重渲先把上一轮产物
移开暂存：**失败原样恢复**（已付费的合格视频/首帧绝不因一轮失败的重渲被销毁），
成功才退役留档（新旧 take 可对比复盘）；重渲轮**强制重新出图**——图像级缺陷在结构上可修。

**预算是预留制，不是事后查账。** 并发 worker 提交付费任务与花费过账之间有时间差，
预估花费先记为"在途"参与门禁判定（`reserve`/`settle`），N 个 worker 无法同时过闸击穿预算；
派生客户端（视觉/媒体/音乐）与母客户端共享同一把锁、同一份账本与批次上下文——
Ctrl-C 取消覆盖全部付费出口。

**门禁 fail-closed。** "没查"绝不能表现为"通过"：字幕越镜核验读不到产物 → 显式失败
（而非静默通过）；响度/峰值测量失败 → 检查项以"未达标"出现在报告里（而非消失）；
峰值测量函数返回错误而不是 0（0 会让"未削波"恒真）。

**实测值优先于计划值。** 视频模型返回固定档位时长（5s/10s），与分镜计划必然漂移。
所有时长、入点、槽位都对实测值计算，字幕与音频跟片走。

**重试是受控操作，不是抽卡。** 种子由 `(镜头ID, 轮次, 角色锚点)` 确定性派生；
同时重渲轮追加提示词变体（按轮次循环取），`rerender_rounds` 调到多大都不会退化成
"悄悄重跑同一提示词"。

**首尾帧夹逼（每镜默认），运镜幅度服从分镜。** 每镜生成视频时都按分镜构图生成一张
"尾帧锚点"，与首帧一起传给 CogVideoX-3 的首尾帧数组——**运动的终点被钉在分镜构图上，
中途不漂离**（`video.end_frame_anchor`，默认开；严格模式：锚点生成/质检失败即镜头失败，不降级）。
链式承接的镜头首帧=上镜末帧、尾帧=本镜构图（接缝连续且运动有终点）；无承接的镜头
（片头/跨场景/定点重渲）首尾两端同为分镜构图，把画面锁死在构图两端。
代价是每镜多一次出图费（约几角钱），`--dry-run` 的预估已计入。参考图生视频路径
（vidu）无尾帧概念，走该路径的镜头自动跳过锚点生成。参考图生视频路径的运动幅度由
运镜枚举映射（固定/缓推→small，横移/摇/手持→medium，航拍/升降/环绕→large），
不再交给模型自定。

**后期消除"镜头间闪一下"。** 合成时实测各镜平均亮度，对偏差镜头施加小幅 `eq` 校正
（±0.06 上限，插在裁切与补边之间、不碰 letterbox 黑边）——只拉齐曝光，不抢调色的戏；
`transitions.match_exposure` 可关。响度归一化用**两遍 loudnorm**（测量 → 线性增益），
单遍动态模式会"泵"音乐尾音、压平对白起伏。

**转场是语法，不是特效。** 叠化（场景切换）与淡入淡出按 editplan 落地，且
**时间轴严格守恒**：叠化占用的 N 秒由 B 侧剩余画面轻微减速（≤1.2×时长）偿还，
成片总时长不变、下游实测时间轴原样有效。不用"末帧冻结"补长——冻结会在剪入
下一镜前产生可见停顿，减速收尾则是剪辑里惯用的"缓缓停下"节奏。
硬切为主、叠化只在场景边界；连续两处叠化按硬约束自动改为硬切（同一视频流不允许被相邻边界双重消费）。

**宽银幕由画幅保证，不由提示词保证。** 生成端按 16:9 出片（模型对 scope 构图的
服从度不可靠），交付时按 shotspec 的 `Style.Aspect`（默认 2.39:1）中心裁切并
letterbox 进交付容器——HD scope 的标准交付形态，字幕烧录在 letterbox 之后、
落在下黑边（影院式字幕位）。`transitions.scope: "off"` 可关。

**同一 run 绝不并发操作。** 运行目录有互斥锁（`.lock`）：两个 render 同时跑会让
断点复用把对方正在写的半截文件当成品。锁超过 6 小时视为崩溃残留自动接管；
`run/resume/render/deliver/polish/takes` 全部持锁进入。

---

## 目录说明

```
cmd/studio/          CLI 入口（run/render/deliver/verify/doctor/resume/status）
internal/config/     生产参数集中配置（模型分层/计费/预算/质检阈值）
internal/promspec/   工业级提示词 DSL（摄影机语言、角色锚点、负面提示词分组）
internal/shotkit/    逐镜头生成计划（参考帧、确定性种子、计划自检）
                    └ assets.go  素材库（场景 establishing 图、译文缓存、索引）
internal/quality/    客观质检（图像/视频指标与三态判定）
internal/render/     逐镜头生成流水线（质检→换种重渲→付费熔断）
internal/narrate/    配音（音色映射、逐句实测、超槽处理、排麦）
internal/audio/      声音后期（多轨混音、侧链闪避、响度归一化、版本兼容）
internal/subtitle/   字幕引擎（实测对齐、折行、CPS 限速、排版校验）
internal/deliver/    交付（完整性门禁、合成、字幕烧录、成片校验、交付报告）
internal/ffx/        ffmpeg 封装（能力探测、版本兼容、严格预检：缺关键能力即阻塞）
internal/engine/     工作流引擎（DAG 并行、重试退避、定向返修、断点续跑）
internal/skills/     10 个创作技能（每个都带本地确定性校验）
internal/selfheal/   规则预过滤 + AI 诊断 + 经验记忆库
internal/web/        Web 控制台（REST API + 内嵌前端；任务以受控子进程走 CLI）
workflows/movie.json 工作流定义
```
