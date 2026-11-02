# cfscan 先行上线：canary 通道

## 背景（已核实）
- 生产节点的前端在运行时更新：`internal/frontend/watcher.go:83` 每 30 分钟调用一次 `FetchFromRolling`，即 `fetcher.go:70` 的 `fetchFromTag(ctx, "rolling", "")`。仓库来自 `conf.Conf.FrontendRepo`（`config.go:126`，cfscan 上是 `Ironboxplus/OpenList-Frontend`）。**因此只要前端 `rolling` release 更新，所有节点都会跟着更新**，做不到只更新 cfscan。
- 后端镜像：`test_docker.yml` 在 push 到 `feat/dynamic-frontend` 时构建 `:front-rolling`。tx、cfscan 都运行这个 tag，所以它等于全员生产。
- **合并回归**：前端合并采纳了上游的 `build_rolling.yml`，发布 tag 从 `rolling` 变成了 `edge`。我们的 fork main（ironbox/main）上仍是 `rolling`。如果原样合入 main，后端拉不到新版，所有节点会悄悄停在旧前端上。

## 改动
1. **后端**：`conf.Config` 加 `FrontendChannel string`（`json:"frontend_channel" env:"FRONTEND_CHANNEL"`），默认 `"rolling"`。`FetchFromRolling` 改为使用这个值，空值按 `rolling` 处理，函数名保持不变。加一个单测，验证 channel 会进入请求的 URL（`fetcher_test.go` 已有 baseURL 注入）。
2. **后端镜像** `test_docker.yml`：push 分支加上 `canary`。`push_image` 里如果 `GITHUB_REF_NAME == canary`，就只打 `:canary` tag，不打 `front-*`、不打 `latest`。`feat/dynamic-frontend` 的行为不变。
3. **前端** `build_rolling.yml`（OpenList-Frontend-ui，feat/ui-shell）：
   - main 发布的 tag 恢复为 `rolling`（与 ironbox/main 一致）。
   - push 分支加上 `canary`，此时 tag 为 `canary`，标题和说明注明是 canary。
   - 删除旧 release 那一步按 tag 删，不会误删另一个通道的 release。
4. **cfscan**（已获批的首个上线目标）：
   - 备份 `compose.yml`。
   - 镜像改为 `ghcr.io/ironboxplus/openlist:canary`，加环境变量 `FRONTEND_CHANNEL=canary`。
   - 执行 `docker compose pull openlist && docker compose up -d openlist`。

## 推进步骤
- canary 分支：后端是 `dev/upstream-2026-11` 的 CI 绿头，前端是 `feat/ui-shell` 的 CI 绿头，分别推到各自仓库的 `canary` 分支。
- 用户在 cfscan 独立验收。
- 通过后提升到全员：
  1. 先把后端快进到 `feat/dynamic-frontend`，构建 `front-rolling` 镜像，逐台升级（tx、.99、本机）。
  2. 再把前端合入 main，发布 `rolling`。
  3. cfscan 改回 `front-rolling`，删掉 `FRONTEND_CHANNEL`。
- 先后端后前端：未定。要等核实"新后端 + 旧前端"能用之后才能定。核实办法：拿旧前端调用的接口清单和新后端的路由做差，在 cfscan 验收期间完成。
