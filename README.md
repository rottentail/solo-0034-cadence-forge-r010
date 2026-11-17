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

检查在临时数据根内生成4秒校准音画与4.5秒含停顿和轻声词尾的口播样例，不依赖外部素材或在线服务，完成25条工作流，包括真实强制终止及恢复。`--workflow`可选择运行清单中的某条工作流；所需的前置接收、版本编辑和渲染也会执行。初始化基线不含自动测试套件，testing为deferred；这里提供有界生产工作流核验，不声明测试命令。

## 调用顺序

1. `POST /forge/v1/sources?name=校准片`：请求体为媒体原始字节，返回素材ID、SHA-256与流事实。
2. `POST /forge/v1/recipes`：例如 `{"name":"审片","format":"mp4","width":640,"height":360,"fps":24,"sample_rate":48000,"normalize":false,"target_lufs":-16}`。
3. `POST /forge/v1/sheets`：`{"name":"剪辑","spans":[{"source_id":"素材ID","in_ms":0,"out_ms":1500,"gain_db":0}],"cues":[{"source_id":"素材ID","start_ms":500,"end_ms":1000,"text":"校准"}]}`。
4. `GET /forge/v1/sheets/{id}/preview?recipe_id={id}`：预览当前最新修订的累计区间及WebVTT。片段按半开区间求交；同源重复出现分别映射。
5. `POST /forge/v1/renders`：提供 `Idempotency-Key`，正文 `{"sheet_id":"剪辑ID","sheet_revision":1,"recipe_id":"配方ID","recipe_revision":1}`。必须显式选择修订，提交冻结引用与时间计划；预览最新版本不替代提交选择。
6. `GET /forge/v1/renders/{id}`：查看排队、运行、成功、失败或取消及尝试历史。
7. `GET /forge/v1/renders/{id}/artifact`：下载检验通过的媒体；`?captions=1`下载字幕。`POST .../verify`重新核验；`GET .../bundle`返回媒体、字幕及冻结说明组成的ZIP。

## 编辑与恢复

`PUT /sheets/{id}`及`PUT /recipes/{id}`使用十进制修订号 `If-Match: 1`。本服务的该头为应用级修订约定，并非通用HTTP条件请求实现。保存只追加历史，过期写入409。输入中的服务管理ID与修订由服务生成。

`POST /renders/{id}/cancel`撤回排队或运行中的交付；成功确认后该任务不能新提交成片。进程停止是后续动作，并不承诺确认瞬间资源已全部释放。成功成片不可再取消。`POST /renders/{id}/retry`仅接受failed，最多三次尝试，沿用原冻结输入。重启时running变为failed/interrupted，等待显式重试，queued继续排队。

`GET /events?after=0`返回至多200条严格后继事件。序号是持久顺序，时间戳仅用于解释。`GET /state`返回当前一致快照，适合受信任客户端。

## 分析入口

`POST /sources/{id}/loudness`测量源LUFS、真峰值和动态范围，静音或不可测结果使用null并指示silent。`GET /sources/{id}/visual?mode=storyboard`生成四格图；`mode=waveform`生成640×120单声道概览。它们是有成本的同步分析，请求受独立超时约束；波形合并声道可能隐藏反相内容，故事板不等于逐帧审片。

## 可采纳的去静音建议

两条流程各自拥有事实，通过修订引用共同维护同一条建议：分析只产生源字节证据，剪辑只决定实际采用的区间。

1. `POST /sources/{id}/silence` 立即登记 `pending` 修订并返回202；分析工作者在后台逐帧测量，`GET /sources/{id}/silence` 列出历史，`GET .../silence/{revision}` 读单次修订。就绪修订绑定源 SHA-256 与字节数，`finished_at` 之后不再改写；重新分析只会追加新修订。分析期间编辑、预览与渲染照常进行。
2. 测量把区间分为 `silence`（有效数字静音，电平可缺失）与 `soft_speech`（可测的低电平声音，如呼吸或轻声词尾）。`soft_speech` 的 `suggested` 始终是 `tradeoff`，不会被当作免费静音。
3. `POST /sheets/{id}/silence:propose?revision=N` 只读地给出可审阅提案：目标时长、四类选项（`keep_all`、`silence_only`、`tighten_pauses`、`silence_and_soft_tails`）各自的结果与缺口、每个测量区间的 `cut`/`keep` 判断及理由、受保护的自然停顿、被裁到或删除的字幕区间。`option: custom` 可逐区间指定实际删除量。纯静音不足时提案报告 `shortfall_ms` 与 `available.max_removable_ms`，并明确提示再删就会碰到语音，而不是自动删掉轻声。
4. `POST /sheets/{id}/silence:adopt` 要求 `If-Match: 基修订`。它在一次耐久更新里追加剪辑修订、采纳记录和所引用的分析修订；若期间有更晚确认的编辑，返回409而不会覆盖。采纳记录保存基/结果修订、两版指纹、目标与缺口、逐区间决定、应用到的确切源区间（含片段序号）与字幕影响。
5. 提交成片时冻结剪辑修订，同时快照采纳记录与分析字节引用；字幕由采纳后的片段经既有交叠映射派生，成片检验校验字幕与冻结计划一致。`GET .../bundle` 在有采纳时额外包含 `silence_trace.json`（采纳记录、分析字节引用与剪辑修订），`GET /state` 与渲染的 `silence_adoption` 字段使最终采用判断可追溯。

分析中断只把 `running` 修订记为 `failed`，已就绪证据与随后编辑不受影响；分析修订上限32次。

## 支持范围与限制

单素材最多64 MiB、约120秒、3840×2160和8声道。剪辑最多32段、120秒、500条字幕，段长至少100ms；输出MP4/H.264/AAC或WAV/16位PCM，采样率44.1或48kHz。MP4要求每段有视频；无声视频生成对应静音。WAV要求每段有声音。视频保持完整画面并留边，统一像素比例与帧率，声音统一立体声。归一化为单遍响度滤镜，不保证任何短片都精确达到目标值。

静音分析以10ms帧统计峰值与RMS：极度安静或不可测的帧归为数字静音，电平以null如实表示；阈值−35 dBFS以下、但高于不可测下限的帧归为低电平语音。测量不等于语义判断：静音检测不区分停顿、气口和轻柔音节，`soft_speech` 只是提示复核；采纳的建议也不保证听感自然，仍需人工审听。默认保留每侧自然停顿400ms（收紧时180ms）并额外留30ms边界；这些是产品默认值，不是对内容正确性的断言。提案只使用已经完成的分析，不会为预览重新运行媒体分析。

时长基准为毫秒，编码后的帧与采样可能有容差。检验检查流、尺寸、采样率、时长与摘要，不代表内容正确、逐帧同步或主观质量已通过人工审片。禁止容器中的远程协议与非白名单封装，但媒体进程仍需要部署侧隔离。

接收先耐久保留字节再探测登记，失败可能留下未引用对象。成功产物先检验、耐久提升，再提交可见事实；崩溃可能留下未登记产物。当前不自动回收这些内容。`DELETE /sources/{id}`只退休无历史引用的素材记录，保守保留对象；历史剪辑、字幕和渲染血缘均阻止退休，无采纳引用的分析修订随素材记录退休。

状态写入使用同文件系统临时文件、fsync、替换与父目录同步；目录同步失败时服务拒绝后续写入并要求恢复核对。对象及产物与状态分阶段提交，已生成目录不能自行视为交付成功。请备份整个数据根，并在停止唯一服务实例后进行一致复制。

## 容器

```sh
docker build -t cadence-forge .
docker run --rm -p 8734:8734 -v cadence-forge-data:/data cadence-forge
```

运行身份为10001。这里的容器定义不是跨平台运行通过声明；实际验证范围与本机工作流结果见交付记录。
