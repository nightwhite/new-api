# Changelog

## Unreleased

### Added
- 新增本地异步包装接口（避免长耗时请求在 CDN 场景触发 524）：
  - `POST /async/v1/chat/completions`（对外保持 `stream=false` 语义）
  - `POST /async/v1/images/generations`
  - `POST /async/v1/images/edits`
  - `GET /async/tasks/:task_id`（任务完成后原样返回对应同步接口的最终响应）
- chat.completions 增加 stream-only 上游自动降级：后台可用 `stream=true` 重新执行并将 SSE 聚合成最终 JSON 后落库。
- async 任务增加执行超时与最大响应体限制：
  - `ASYNC_TASK_TIMEOUT_SECONDS`（默认 600）
  - `ASYNC_TASK_MAX_RESPONSE_MB`（默认 32）

### Changed
- `Task` 表新增字段 `response_status_code`（用于 async 包装任务保存原始 HTTP 状态码）。
- 任务轮询逻辑跳过 `async_relay` 平台，避免被视频/第三方任务轮询误处理。
- GitHub Actions：推送到分支 `night` 会构建并推送 GHCR 镜像（tag 前缀 `night`）。

