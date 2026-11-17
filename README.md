# Cadence Forge · 音视频成片服务

Go HTTP 后端，以真实音视频字节为输入，提供版本化剪辑、字幕时间映射、MP4/WAV加工和可核验交付。没有Web前端。

## 运行

需要 Go 1.24+、Python 3（仅运行有界工作流检查）及带 libx264、AAC、loudnorm、tile、showwavespic 的 FFmpeg/ffprobe。当前本机实际验证 Go 1.27、FFmpeg/ffprobe 9.0.2；Docker构建定义采用Go 1.24和Debian媒体包，镜像运行尚需目标环境独立验证。

```sh
go build -trimpath -o bin/forged ./cmd/forged
./bin/forged
```

默认监听 `127.0.0.1:8734`，数据根 `var/cadence-forge`。可以设置 `FORGE_ADDR`、`FORGE_ROOT`、`FORGE_WORKERS`（1..8，默认2）、`FFMPEG_BIN`、`FFPROBE_BIN`。同一数据根仅允许一个服务进程。服务作为受信任环境中的媒体后端运行，面向公网接入前应部署认证和网络边界。

使用现有媒体工具运行所有真实工作流：

```sh
python3 scripts/workflow_check.py --workflow all
```

检查在临时数据根内生成4秒校准音画，不依赖外部素材或在线服务，完成19条工作流，包括真实强制终止及恢复。`--workflow`可选择运行清单中的某条工作流；所需的前置接收、版本编辑和渲染也会执行。另有 `python3 scripts/transition_check.py` 用独立的多素材、重复源片段与无声视频对交叠转场做端到端核对。初始化基线不含自动测试套件，testing为deferred；这里提供有界生产工作流核验，不声明测试命令。

## 交叠转场与取舍说明

剪辑单可在 `transitions` 中为相邻片段边界声明期望交叠（毫秒），并用 `markers` 标注源时间上的关键画面；`cues` 中的对白区间同样按源时间标注。示例：

```json
{"name":"剪辑","spans":[{"source_id":"A","in_ms":0,"out_ms":2000,"gain_db":0},
  {"source_id":"B","in_ms":0,"out_ms":2000,"gain_db":0}],
 "transitions":[{"before_index":1,"requested_ms":500}],
 "markers":[{"source_id":"B","at_ms":150,"label":"关键画面"}],
 "cues":[{"source_id":"B","start_ms":600,"end_ms":1100,"text":"完整对白"}]}
```

预览与渲染计划中的每个边界返回 `requested_ms`、`effective_ms` 与 `reasons`：

- 能完整承担时 `effective_ms` 等于期望值，`reasons` 为空；
- 溶接窗口若覆盖对白或关键画面，交叠被压到最大安全值，原因记为 `protected_content`；
- 短中间片段的前后转场争用同一画面时，进入侧先取可用量，离开侧按剩余预算压缩，原因记为 `contention_clamped`；片段本身短于期望时记为 `span_too_short`；
- 关键画面落在片段首帧等极端情况下交叠解析为0并解释，剪辑不会被拒绝；结构非法（边界越界、重复、交叠超过10000ms）才在保存时返回400。

这是有意选择的可审阅取舍：不悄悄缩短（实际值和原因随预览返回、随渲染冻结、写进交付包 `composition.json`），也不一概拒绝合法剪辑。交叠只缩短成片时长，不切去任何已选源画面；每段至少保留1ms独有画面。MP4用真实 `xfade`（画面）与 `acrossfade`（声音）滤镜实现，WAV无画面可溶接，交叠记录为 `format_unsupported` 并保持声音连续拼接。无声视频保留全部画面并合成等长立体静音。

## 调用顺序

1. `POST /forge/v1/sources?name=校准片`：请求体为媒体原始字节，返回素材ID、SHA-256与流事实。
2. `POST /forge/v1/recipes`：例如 `{"name":"审片","format":"mp4","width":640,"height":360,"fps":24,"sample_rate":48000,"normalize":false,"target_lufs":-16}`。
3. `POST /forge/v1/sheets`：`{"name":"剪辑","spans":[{"source_id":"素材ID","in_ms":0,"out_ms":1500,"gain_db":0}],"cues":[{"source_id":"素材ID","start_ms":500,"end_ms":1000,"text":"校准"}]}`。
4. `GET /forge/v1/sheets/{id}/preview?recipe_id={id}`：预览当前最新修订的累计区间、交叠转场决策及WebVTT。片段按半开区间求交；同源重复出现分别映射。
5. `POST /forge/v1/renders`：提供 `Idempotency-Key`，正文 `{"sheet_id":"剪辑ID","sheet_revision":1,"recipe_id":"配方ID","recipe_revision":1}`。必须显式选择修订，提交冻结引用与时间计划；预览最新版本不替代提交选择。
6. `GET /forge/v1/renders/{id}`：查看排队、运行、成功、失败或取消及尝试历史。
7. `GET /forge/v1/renders/{id}/artifact`：下载检验通过的媒体；`?captions=1`下载字幕。`POST .../verify`重新核验；`GET .../bundle`返回媒体、字幕及冻结说明组成的ZIP。

## 编辑与恢复

`PUT /sheets/{id}`及`PUT /recipes/{id}`使用十进制修订号 `If-Match: 1`。本服务的该头为应用级修订约定，并非通用HTTP条件请求实现。保存只追加历史，过期写入409。输入中的服务管理ID与修订由服务生成。

`POST /renders/{id}/cancel`撤回排队或运行中的交付；成功确认后该任务不能新提交成片。进程停止是后续动作，并不承诺确认瞬间资源已全部释放。成功成片不可再取消。`POST /renders/{id}/retry`仅接受failed，最多三次尝试，沿用原冻结输入。重启时running变为failed/interrupted，等待显式重试，queued继续排队。

`GET /events?after=0`返回至多200条严格后继事件。序号是持久顺序，时间戳仅用于解释。`GET /state`返回当前一致快照，适合受信任客户端。

## 分析入口

`POST /sources/{id}/loudness`测量源LUFS、真峰值和动态范围，静音或不可测结果使用null并指示silent。`GET /sources/{id}/visual?mode=storyboard`生成四格图；`mode=waveform`生成640×120单声道概览。它们是有成本的同步分析，请求受独立超时约束；波形合并声道可能隐藏反相内容，故事板不等于逐帧审片。

## 支持范围与限制

单素材最多64 MiB、约120秒、3840×2160和8声道。剪辑最多32段、120秒、500条字幕和200个关键画面标记，段长至少100ms；交叠转场按相邻边界声明，单边界期望交叠0..10000ms。输出MP4/H.264/AAC或WAV/16位PCM，采样率44.1或48kHz。MP4要求每段有视频；无声视频生成对应静音。WAV要求每段有声音。视频保持完整画面并留边，统一像素比例与帧率，声音统一立体声。归一化为单遍响度滤镜，不保证任何短片都精确达到目标值。

时长基准为毫秒，编码后的帧与采样可能有容差。检验检查流、尺寸、采样率、时长与摘要，不代表内容正确、逐帧同步或主观质量已通过人工审片。禁止容器中的远程协议与非白名单封装，但媒体进程仍需要部署侧隔离。

接收先耐久保留字节再探测登记，失败可能留下未引用对象。成功产物先检验、耐久提升，再提交可见事实；崩溃可能留下未登记产物。当前不自动回收这些内容。`DELETE /sources/{id}`只退休无历史引用的素材记录，保守保留对象；历史剪辑、字幕和渲染血缘均阻止退休。

状态写入使用同文件系统临时文件、fsync、替换与父目录同步；目录同步失败时服务拒绝后续写入并要求恢复核对。对象及产物与状态分阶段提交，已生成目录不能自行视为交付成功。请备份整个数据根，并在停止唯一服务实例后进行一致复制。

## 容器

```sh
docker build -t cadence-forge .
docker run --rm -p 8734:8734 -v cadence-forge-data:/data cadence-forge
```

运行身份为10001。这里的容器定义不是跨平台运行通过声明；实际验证范围与本机工作流结果见交付记录。
